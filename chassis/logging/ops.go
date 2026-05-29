package logging

import (
	"encoding/json"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// WriteOpsTop Log In/Out for RID
func WriteOpsTop(config *config.Config, rid string, raw string, payload *event.Payload) error {
	rid = strings.Replace(rid, "..", "", -1) // rid could come from request? ../
	rid = strings.Replace(rid, "/", "_", -1) //

	// TODO config.LogOps = logger
	if config.LogOps == "dir" {
		root := path.Join(config.LogOpsDir, rid)
		err := os.MkdirAll(root, os.ModePerm)
		if err != nil {
			return err
		}

		if raw != "" {
			infilename := path.Join(root, "in.json")
			infile, err := os.OpenFile(infilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer infile.Close()
			in := gjson.Get(raw, "@pretty")
			if _, err = infile.WriteString(in.String() + "\n"); err != nil {
				return err
			}
		}

		if payload != nil {
			outfilename := path.Join(root, "out.json") // not necessarily json?
			outfile, err := os.OpenFile(outfilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer outfile.Close()

			out := ""
			if payload.Type == event.JSON {
				out = gjson.Get(payload.Raw, "@pretty").String()
			} else {
				o, _ := json.MarshalIndent(payload, "", "\t")
				out = string(o)
			}
			if _, err = outfile.WriteString(out + "\n"); err != nil {
				return err
			}
		}
	}

	return nil
}

// WriteOps Write operation to logs
func WriteOps(logger *zap.Logger, config *config.Config, rid string, opName string, op operation.Operation, payload event.Payload, duration int64) error {

	if config.LogOps == "logger" {
		logger.Debug(
			"exec",
			zap.String("opName", opName),
			zap.String("opId", op.OpID),
			zap.String("stack", op.Stack),
			zap.Int("scope", op.Scope),
			zap.String("rid", rid),
			zap.String("in", string(op.Input)),
			zap.String("out", string(payload.Raw)),
			zap.Int64("duration", duration),
		)
	} else if config.LogOps == "dir" {
		rid = strings.Replace(rid, "..", "", -1) // rid could come from request? ../
		rid = strings.Replace(rid, "/", "_", -1) //
		// TODO: staticcheck - I think we don't need this at the moment
		//strings.Replace(opName, "/", "_", -1) //
		scope := strconv.Itoa(op.Scope)
		stack := strings.Replace(op.Stack, "/", "_", -1) //
		opID := strings.Replace(op.OpID, "/", "_", -1)   //

		root := path.Join(config.LogOpsDir, rid)
		stepDir := path.Join(root, scope, stack, opID)

		err := os.MkdirAll(root, os.ModePerm)
		if err != nil {
			return err
		}
		err = os.MkdirAll(stepDir, os.ModePerm)
		if err != nil {
			return err
		}

		// logdir/$rid/$scope/$opname/{in,out}.json

		infilename := path.Join(stepDir, "in.json")
		outfilename := path.Join(stepDir, "out.json")
		resonatorfilename := path.Join(stepDir, "resonator.txcl")

		infile, err := os.OpenFile(infilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer infile.Close()

		outfile, err := os.OpenFile(outfilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer outfile.Close()

		resfile, err := os.OpenFile(resonatorfilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer resfile.Close()

		if (op.Meta != "") || (payload.Meta != "") {
			metafilename := path.Join(stepDir, "meta.json")
			metafile, err := os.OpenFile(metafilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer metafile.Close()

			if op.Meta != "" {
				if _, err = metafile.WriteString(string(op.Meta) + "\n"); err != nil {
					return err
				}
			}
			if payload.Meta != "" {
				meta := gjson.Get(payload.Meta, "@pretty")
				if _, err = metafile.WriteString(meta.String() + "\n"); err != nil {
					return err
				}
			}
		}

		in := gjson.Get(op.Input, "@pretty")
		if _, err = infile.WriteString(in.String() + "\n"); err != nil {
			return err
		}

		//  out, _ := json.MarshalIndent(payload, "", "\t")
		out := gjson.Get(payload.Raw, "@pretty")
		if _, err = outfile.WriteString(out.String() + "\n"); err != nil {
			return err
		}

		if _, err = resfile.WriteString(string(op.Txcl) + "\n"); err != nil {
			return err
		}
	}
	return nil
}
