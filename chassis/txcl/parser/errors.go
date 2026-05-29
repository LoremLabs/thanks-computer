package parser

import (
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
)

func (p *Parser) Errors() []string {
	return p.errors
}

func (p *Parser) curError(t token.TokenType) {
	msg := fmt.Sprintf("expected token to be %s, got %s instead",
		t, p.curToken.Type)
	p.errors = append(p.errors, msg)
}

func (p *Parser) wrongTypeParseError(expecting string) {
	msg := fmt.Sprintf("Expecting %s, received %s", expecting, p.curToken.Literal)
	p.errors = append(p.errors, msg)
}
