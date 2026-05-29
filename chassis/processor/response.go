package processor

import (
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

func (pu *Unit) MakeMockResponse(op operation.Operation, errMsg string) event.Payload {

	pu.Logger.Info("using mock", zap.String("mockReason", errMsg))

	if errMsg != "" {
		op.Meta, _ = sjson.Set(op.Meta, "error", errMsg)
	}
	pl := event.Payload{
		Raw:  `{}`,
		Type: event.Null,
		Meta: op.Meta,
	}

	if len(op.MockRes) > 0 {
		pl.Raw = op.MockRes
		op.Meta, _ = sjson.Set(op.Meta, "mock.-1", op.Resonator.Exec)
		pl.Type = event.JSON
		pl.Meta = op.Meta
	}

	return pl
}
