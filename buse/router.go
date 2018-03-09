package buse

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/itchio/butler/database"
	"github.com/itchio/butler/progress"

	"github.com/go-errors/errors"
	"github.com/itchio/butler/comm"
	"github.com/itchio/butler/database/models"
	"github.com/itchio/butler/mansion"
	itchio "github.com/itchio/go-itchio"
	"github.com/itchio/wharf/state"
	"github.com/jinzhu/gorm"
	"github.com/sourcegraph/jsonrpc2"
)

type RequestHandler func(rc *RequestContext) (interface{}, error)

type Router struct {
	Handlers       map[string]RequestHandler
	MansionContext *mansion.Context
	CancelFuncs    *CancelFuncs
	openDB         OpenDBFunc
}

type OpenDBFunc func() (*gorm.DB, error)

func NewRouter(mansionContext *mansion.Context, openDB OpenDBFunc) *Router {
	return &Router{
		Handlers:       make(map[string]RequestHandler),
		MansionContext: mansionContext,
		CancelFuncs: &CancelFuncs{
			Funcs: make(map[string]context.CancelFunc),
		},

		openDB: openDB,
	}
}

func (r *Router) Register(method string, rh RequestHandler) {
	if _, ok := r.Handlers[method]; ok {
		panic(fmt.Sprintf("Can't register handler twice for %s", method))
	}
	r.Handlers[method] = rh
}

func (r Router) Dispatch(ctx context.Context, origConn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	method := req.Method
	var res interface{}

	conn := &jsonrpc2Conn{origConn}
	consumer, cErr := NewStateConsumer(&NewStateConsumerParams{
		Ctx:  ctx,
		Conn: conn,
	})
	if cErr != nil {
		return
	}

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if rErr, ok := r.(error); ok {
					err = errors.Wrap(rErr, 0)
				} else {
					err = errors.New(r)
				}
			}
		}()

		if h, ok := r.Handlers[method]; ok {
			var _db *gorm.DB
			getDB := func() *gorm.DB {
				if _db == nil {
					db, err := r.openDB()
					if err != nil {
						panic(err)
					}

					database.SetLogger(db, consumer)

					_db = db
				}
				return _db
			}
			defer func() {
				if _db != nil {
					err := _db.Close()
					if err != nil {
						comm.Warnf("Could not close db connection: %s", err.Error())
					}
				}
			}()

			rc := &RequestContext{
				Ctx:            ctx,
				Harness:        NewProductionHarness(),
				Consumer:       consumer,
				Params:         req.Params,
				Conn:           conn,
				MansionContext: r.MansionContext,
				CancelFuncs:    r.CancelFuncs,
				DB:             getDB,
			}

			rc.Consumer.OnProgress = func(alpha float64) {
				if rc.counter == nil {
					// skip
					return
				}

				rc.counter.SetProgress(alpha)
				notif := &ProgressNotification{
					Progress: alpha,
					ETA:      rc.counter.ETA().Seconds(),
					BPS:      rc.counter.BPS(),
				}
				// cannot use autogenerated wrappers to avoid import cycles
				rc.Notify("Progress", notif)
			}
			rc.Consumer.OnProgressLabel = func(label string) {
				// muffin
			}
			rc.Consumer.OnPauseProgress = func() {
				if rc.counter != nil {
					rc.counter.Pause()
				}
			}
			rc.Consumer.OnResumeProgress = func() {
				if rc.counter != nil {
					rc.counter.Resume()
				}
			}

			res, err = h(rc)
		} else {
			err = StandardRpcError(jsonrpc2.CodeMethodNotFound)
		}
		return
	}()

	if err == nil {
		err = origConn.Reply(ctx, req.ID, res)
		if err != nil {
			consumer.Errorf("Error while replying: %s", err.Error())
		}
		return
	}

	if ee, ok := AsBuseError(err); ok {
		origConn.ReplyWithError(ctx, req.ID, ee.AsJsonRpc2())
		return
	}

	var rawData *json.RawMessage
	if se, ok := err.(*errors.Error); ok {
		input := map[string]interface{}{
			"stack":         se.ErrorStack(),
			"butlerVersion": r.MansionContext.VersionString,
		}
		es, err := json.Marshal(input)
		if err == nil {
			rm := json.RawMessage(es)
			rawData = &rm
		}
	}
	origConn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
		Code:    jsonrpc2.CodeInternalError,
		Message: err.Error(),
		Data:    rawData,
	})
}

type RequestContext struct {
	Ctx            context.Context
	Harness        Harness
	Consumer       *state.Consumer
	Params         *json.RawMessage
	Conn           Conn
	MansionContext *mansion.Context
	CancelFuncs    *CancelFuncs
	DB             DBGetter

	notificationInterceptors map[string]NotificationInterceptor
	counter                  *progress.Counter
}

type DBGetter func() *gorm.DB

type WithParamsFunc func() (interface{}, error)

type NotificationInterceptor func(method string, params interface{}) error

func (rc *RequestContext) Call(method string, params interface{}, res interface{}) error {
	return rc.Conn.Call(rc.Ctx, method, params, res)
}

func (rc *RequestContext) InterceptNotification(method string, interceptor NotificationInterceptor) {
	if rc.notificationInterceptors == nil {
		rc.notificationInterceptors = make(map[string]NotificationInterceptor)
	}
	rc.notificationInterceptors[method] = interceptor
}

func (rc *RequestContext) StopInterceptingNotification(method string) {
	if rc.notificationInterceptors == nil {
		return
	}
	delete(rc.notificationInterceptors, method)
}

func (rc *RequestContext) Notify(method string, params interface{}) error {
	if rc.notificationInterceptors != nil {
		if ni, ok := rc.notificationInterceptors[method]; ok {
			return ni(method, params)
		}
	}
	return rc.Conn.Notify(rc.Ctx, method, params)
}

func (rc *RequestContext) RootClient() (*itchio.Client, error) {
	return rc.KeyClient("<keyless>")
}

func (rc *RequestContext) KeyClient(key string) (*itchio.Client, error) {
	return rc.MansionContext.NewClient(key)
}

func (rc *RequestContext) ProfileClient(profileID int64) (*models.Profile, *itchio.Client) {
	if profileID == 0 {
		panic(errors.New("profileId must be non-zero"))
	}

	profile := models.ProfileByID(rc.DB(), profileID)
	if profile == nil {
		panic(errors.Errorf("Could not find profile %d", profileID))
	}

	if profile.APIKey == "" {
		panic(errors.Errorf("Profile %d lacks API key", profileID))
	}

	client, err := rc.MansionContext.NewClient(profile.APIKey)
	if err != nil {
		panic(errors.Wrap(err, 0))
	}

	return profile, client
}

func (rc *RequestContext) StartProgress() {
	rc.StartProgressWithTotalBytes(0)
}

func (rc *RequestContext) StartProgressWithTotalBytes(totalBytes int64) {
	rc.StartProgressWithInitialAndTotal(0.0, totalBytes)
}

func (rc *RequestContext) StartProgressWithInitialAndTotal(initialProgress float64, totalBytes int64) {
	rc.Consumer.Infof("Starting progress (initial %.2f, totalBytes %d)", initialProgress, totalBytes)

	if rc.counter != nil {
		rc.Consumer.Warnf("Asked to start progress but already tracking progress!")
		return
	}

	rc.counter = progress.NewCounter()
	rc.counter.SetSilent(true)
	rc.counter.SetProgress(initialProgress)
	rc.counter.SetTotalBytes(totalBytes)
	rc.counter.Start()
}

func (rc *RequestContext) EndProgress() {
	if rc.counter != nil {
		rc.counter.Finish()
		rc.counter = nil
	} else {
		rc.Consumer.Warnf("Asked to stop progress but wasn't tracking progress!")
	}
}

type CancelFuncs struct {
	Funcs map[string]context.CancelFunc
}

func (cf *CancelFuncs) Add(id string, f context.CancelFunc) {
	cf.Funcs[id] = f
}

func (cf *CancelFuncs) Remove(id string) {
	delete(cf.Funcs, id)
}

func (cf *CancelFuncs) Call(id string) bool {
	if f, ok := cf.Funcs[id]; ok {
		f()
		delete(cf.Funcs, id)
		return true
	}

	return false
}
