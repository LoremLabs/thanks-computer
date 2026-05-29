package repl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/parser"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
)

const PROMPT = ">> "

func Start(in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	//	env := object.NewEnvironment()

	count := 0

	for {
		fmt.Print(PROMPT)
		scanned := scanner.Scan()
		if !scanned {
			return
		}

		line := scanner.Text() // nolint
		// <query> :=
		//
		// [WHEN   ( "*"
		//         | <compound-condition>+
		//         )
		// ]
		// [STEP <step-expression>+]
		// [HAVING <wasm-name>]
		// [SET <event-path> = <value>]
		// [SELECT ( "*"
		//         | <event-property-filter>+
		//         )
		// ]
		// [SET <event-path> = <value>]
		// [WITH <with-modifiers>]
		// [PRIORITY <priority>]
		// [EXEC ( <opstack>
		//         LOOKUP(<opstack-name>)
		//       )
		// ]

if (false) {
		switch count {
		case 1:
			line = `WHEN .foo > 123 SELECT .web, .thing, .another.thing, .moo STEP > 100, < 200, == 500 SET .b = 9, .multi.level = "apple" PRIORITY 100 WITH moo = 1 EXEC "hello-world"`
		case 2:
			line = `WHEN * SET .a = 7, .b = "moose" SELECT @x AS .y SET .c = false PRIORITY 1 EXEC "hello-world"`
		case 3:
			line = `WHEN BUCKET(.thing) != 1000 PRIORITY 1 EXEC "hello-world"`
		case 4:
			line = `WHEN * HAVING "wasm-name-here" SET .manual = true, .multi.level = "asdf", .string = "yup", .number = 7 SELECT @x AS .y SET .number = 8 WITH timeout = 1000, stackName = "canary" PRIORITY 10 EXEC "hello"`
		case 5:
			line = `WHEN * SELECT @x AS .y SET .b = 9, .multi.level = "apple" PRIORITY 100 EXEC "hello-world"`
		case 6:
			line = `WHEN * SELECT @x AS .y SET .b = 9, .multi.level = "apple" PRIORITY 100 EXEC "hello-world"`
		default: // 0 too
			line = `WHEN .a != "abc", BUCKET(.c) > 5, .b == null HAVING "having-here" STEP == 100 SELECT .abc, .xyz WITH abc = "def", GHI = 123 PRIORITY 1 EXEC "hello-world"`
		}

		count++
		count = count % 6
}
		l := lexer.New(line)
		_, err := io.WriteString(out, line)
		if err != nil {
			fmt.Printf("write err")
		}
		for tok := l.NextToken(); tok.Type != token.EOF; tok = l.NextToken() {
			fmt.Printf("%+v\n", tok)
		}
		l = lexer.New(line) // reset for parser

		// rawEvent := []byte(`{"num":6.13,"strs":["a","b"]}`)
		// e, err := event.NewJSON(rawEvent)
		// if err != nil {
		// 	fmt.Printf("parser error %s", err)
		// 	continue
		// }

		p := parser.New(l)
		resonator := p.ParseEvent()
		debug, _ := json.MarshalIndent(resonator, "", " ")
		fmt.Printf("resonator %s", debug)

		/*
			we will have multiple resonators,

			if the select statement matches (and eventually the having) we add this resonator to the potential list
			3 functions: 1) generate data structure, 2) see if structure matches an event, 3) get modified event

			we'll then need to pick the resonator with the highest matching priority and run to get the modified event, exec, etc.
		*/

		if len(p.Errors()) != 0 {
			printParserErrors(out, p.Errors())
			continue
		}

		// evaluated := evaluator.Eval(program, env)
		// if evaluated != nil {
		// 	io.WriteString(out, "\n")
		// 	io.WriteString(out, evaluated.Inspect())
		// 	io.WriteString(out, "\n")
		// } else {
		// 	fmt.Printf("evaluated nil")
		// }

	}
}

func printParserErrors(out io.Writer, errors []string) {
	io.WriteString(out, " parser errors:\n") // nolint
	for _, msg := range errors {
		io.WriteString(out, "\t"+msg+"\n") // nolint
	}
}
