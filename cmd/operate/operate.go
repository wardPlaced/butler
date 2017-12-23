package operate

import (
	"context"
	"fmt"

	"github.com/go-errors/errors"
	"github.com/itchio/butler/buse"
	"github.com/itchio/butler/comm"
	"github.com/itchio/butler/mansion"
	"github.com/sourcegraph/jsonrpc2"
)

func Start(ctx context.Context, mansionContext *mansion.Context, conn *jsonrpc2.Conn, params *buse.OperationStartParams) (*buse.OperationResult, error) {
	if params.StagingFolder == "" {
		return nil, errors.New("No staging folder specified")
	}

	oc := LoadContext(conn, ctx, mansionContext, comm.NewStateConsumer(), params.StagingFolder)

	meta := &MetaSubcontext{
		data: params,
	}

	oc.Load(meta)

	if meta.data.Operation == "" {
		return nil, errors.New("No operation specified")
	}

	oc.Save(meta)

	switch params.Operation {
	case "install":
		ires, err := install(oc, meta)
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}

		oc.consumer.Infof("Installed %d files, reporting success", len(ires.Files))

		err = oc.Retire()
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}

		return &buse.OperationResult{
			Success: true,
			InstallResult: &buse.InstallResult{
				Game:   params.InstallParams.Game,
				Upload: params.InstallParams.Upload,
				Build:  params.InstallParams.Build,
			},
		}, nil
	}

	return nil, fmt.Errorf("Unknown operation '%s'", params.Operation)
}

type MetaSubcontext struct {
	data *buse.OperationStartParams
}

var _ Subcontext = (*MetaSubcontext)(nil)

func (mt *MetaSubcontext) Key() string {
	return "meta"
}

func (mt *MetaSubcontext) Data() interface{} {
	return &mt.data
}
