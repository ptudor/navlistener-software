package ingest

import "github.com/ptudor/navlistener/internal/wire"

type BoardUpdate = wire.UpdateStatus

func DecodeBoardUpdate(bytes []byte) (*BoardUpdate, error) {
	status, err := wire.DecodeUpdateStatus(bytes)
	if err != nil {
		return nil, ErrBadTelemetry
	}
	return status, nil
}
