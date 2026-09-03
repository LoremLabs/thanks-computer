package imap

import "go.opentelemetry.io/otel/attribute"

func attrOutcome(v string) attribute.KeyValue { return attribute.String("txco.imap.outcome", v) }
