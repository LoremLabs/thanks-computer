package imap

import "go.opentelemetry.io/otel/attribute"

func attrOutcome(v string) attribute.KeyValue { return attribute.String("txco.imap.outcome", v) }

// attrBare marks a LOGIN whose username had no @ (see Controller.noteLogin).
func attrBare(v bool) attribute.KeyValue { return attribute.Bool("txco.imap.bare", v) }
