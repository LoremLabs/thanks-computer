package decide

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func ptr(f float64) *float64 { return &f }

func TestParseState(t *testing.T) {
	for _, ok := range []string{`"a message"`, `{"order":{"id":"A-1"}}`, `[{"role":"user"}]`} {
		got, err := ParseState(gjson.Parse(ok))
		if err != nil {
			t.Errorf("ParseState(%s): %v", ok, err)
			continue
		}
		if string(got) != ok {
			t.Errorf("ParseState(%s) = %s, want unchanged", ok, got)
		}
	}
	for _, bad := range []string{`null`, `""`, `"   "`, `42`, `true`} {
		if _, err := ParseState(gjson.Parse(bad)); err == nil {
			t.Errorf("ParseState(%s): want error", bad)
		}
	}
	if _, err := ParseState(gjson.Get(`{}`, "state")); err == nil {
		t.Error("missing state: want error")
	}
}

func TestParseQuestionsAllTypesKeepOrder(t *testing.T) {
	raw := `{
	  "folder":    {"type":"choice","instructions":"Which folder?",
	                "criteria":{"Invoices":"bills","Contracts":"agreements","Letters":""}},
	  "automated": {"type":"noul","instructions":"Sent by a machine?",
	                "criteria":{"true":"newsletters","false":"a person"}},
	  "bare":      {"type":"noul","instructions":"Is it urgent?"},
	  "urgency":   {"type":"score","instructions":"How urgent?","criteria":["low","medium","high"]}
	}`
	qs, err := ParseQuestions(gjson.Parse(raw))
	if err != nil {
		t.Fatalf("ParseQuestions: %v", err)
	}
	keys := []string{}
	for _, q := range qs {
		keys = append(keys, q.Key)
	}
	if strings.Join(keys, ",") != "folder,automated,bare,urgency" {
		t.Fatalf("question order = %v, want author order", keys)
	}
	names := []string{}
	for _, o := range qs[0].Choices {
		names = append(names, o.Name)
	}
	if strings.Join(names, ",") != "Invoices,Contracts,Letters" || qs[0].Choices[0].Description != "bills" {
		t.Fatalf("choices = %+v, want author order with descriptions", qs[0].Choices)
	}
	if qs[1].True != "newsletters" || qs[1].False != "a person" {
		t.Fatalf("noul criteria = %+v", qs[1])
	}
	if qs[2].True != "" || qs[2].False != "" {
		t.Fatalf("noul without criteria = %+v", qs[2])
	}
	if strings.Join(qs[3].Levels, ",") != "low,medium,high" {
		t.Fatalf("levels = %v", qs[3].Levels)
	}
}

func TestParseQuestionsRejects(t *testing.T) {
	cases := map[string]string{
		"not an object":         `["x"]`,
		"empty":                 `{}`,
		"key with a dot":        `{"a.b":{"type":"noul","instructions":"x"}}`,
		"key with a space":      `{"a b":{"type":"noul","instructions":"x"}}`,
		"key starts with dash":  `{"-a":{"type":"noul","instructions":"x"}}`,
		"duplicate key":         `{"a":{"type":"noul","instructions":"x"},"a":{"type":"noul","instructions":"y"}}`,
		"question not object":   `{"a":"is it?"}`,
		"unknown field":         `{"a":{"type":"choice","instructions":"x","options":{"y":"z"}}}`,
		"missing type":          `{"a":{"instructions":"x"}}`,
		"unknown type":          `{"a":{"type":"boolean","instructions":"x"}}`,
		"missing instructions":  `{"a":{"type":"noul"}}`,
		"blank instructions":    `{"a":{"type":"noul","instructions":"  "}}`,
		"noul criteria key":     `{"a":{"type":"noul","instructions":"x","criteria":{"maybe":"y"}}}`,
		"noul criteria array":   `{"a":{"type":"noul","instructions":"x","criteria":["yes","no"]}}`,
		"noul criteria value":   `{"a":{"type":"noul","instructions":"x","criteria":{"true":1}}}`,
		"choice no criteria":    `{"a":{"type":"choice","instructions":"x"}}`,
		"choice array criteria": `{"a":{"type":"choice","instructions":"x","criteria":["a","b"]}}`,
		"choice no options":     `{"a":{"type":"choice","instructions":"x","criteria":{}}}`,
		"choice blank name":     `{"a":{"type":"choice","instructions":"x","criteria":{"":"y"}}}`,
		"choice duplicate name": `{"a":{"type":"choice","instructions":"x","criteria":{"y":"1","y":"2"}}}`,
		"choice non-string":     `{"a":{"type":"choice","instructions":"x","criteria":{"y":2}}}`,
		"score one level":       `{"a":{"type":"score","instructions":"x","criteria":["only"]}}`,
		"score object":          `{"a":{"type":"score","instructions":"x","criteria":{"lo":"","hi":""}}}`,
		"score blank level":     `{"a":{"type":"score","instructions":"x","criteria":["lo",""]}}`,
	}
	for name, raw := range cases {
		_, err := ParseQuestions(gjson.Parse(raw))
		var iw *InvalidWithError
		if !errors.As(err, &iw) {
			t.Errorf("%s: err = %v, want *InvalidWithError", name, err)
		}
	}
	if _, err := ParseQuestions(gjson.Get(`{}`, "questions")); err == nil {
		t.Error("missing questions: want error")
	}
}

func testRequest(t *testing.T) Request {
	t.Helper()
	qs, err := ParseQuestions(gjson.Parse(`{
	  "automated": {"type":"noul","instructions":"Machine?"},
	  "folder":    {"type":"choice","instructions":"Folder?","criteria":{"Invoices":"","Contracts":""}},
	  "urgency":   {"type":"score","instructions":"Urgency?","criteria":["low","medium","high"]}
	}`))
	if err != nil {
		t.Fatalf("ParseQuestions: %v", err)
	}
	return Request{State: []byte(`"x"`), Questions: qs}
}

func goodAnswers() []Answer {
	// Deliberately out of question order, with extras the provider made up.
	return []Answer{
		{Key: "urgency", Type: TypeScore, Score: ptr(1.4), Probabilities: map[string]float64{"0": 0.1, "1": 0.4, "2": 0.5, "9": 0.2}},
		{Key: "folder", Type: TypeChoice, Choice: "Invoices", Probability: ptr(0.5), Confidence: ptr(0.88),
			Probabilities: map[string]float64{"Invoices": 0.91, "Contracts": 0.09, "Receipts": 0.3}},
		{Key: "automated", Type: TypeNoul, Probability: ptr(0.97), Choice: "junk"},
		{Key: "unasked", Type: TypeNoul, Probability: ptr(0.5)},
	}
}

func TestNormalizeHappyPath(t *testing.T) {
	req := testRequest(t)
	resp := Response{Answers: goodAnswers()}
	if err := Normalize(req, &resp); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("answers = %d, want 3 (unasked dropped)", len(resp.Answers))
	}
	noul, choice, score := resp.Answers[0], resp.Answers[1], resp.Answers[2]
	if noul.Key != "automated" || choice.Key != "folder" || score.Key != "urgency" {
		t.Fatalf("answers not in question order: %v %v %v", noul.Key, choice.Key, score.Key)
	}
	if *noul.Probability != 0.97 || noul.Choice != "" || noul.Probabilities != nil {
		t.Errorf("noul = %+v, want only P(true)", noul)
	}
	// P(choice) comes from the distribution, not the provider's stray field.
	if choice.Choice != "Invoices" || *choice.Probability != 0.91 {
		t.Errorf("choice = %+v, want Invoices @ 0.91", choice)
	}
	if choice.Confidence == nil || *choice.Confidence != 0.88 {
		t.Errorf("reported confidence not carried: %+v", choice)
	}
	if noul.Confidence != nil || score.Confidence != nil {
		t.Errorf("confidence invented where none was reported: %+v %+v", noul, score)
	}
	if _, ok := choice.Probabilities["Receipts"]; ok || len(choice.Probabilities) != 2 {
		t.Errorf("choice distribution = %v, want only offered names", choice.Probabilities)
	}
	if score.Probability != nil {
		t.Errorf("score must carry no synthetic probability: %+v", score)
	}
	if *score.Score != 1.4 || len(score.Probabilities) != 3 {
		t.Errorf("score = %+v, want 1.4 with levels 0-2 only", score)
	}
}

func TestNormalizeRejects(t *testing.T) {
	mutate := map[string]func([]Answer) []Answer{
		"missing answer": func(a []Answer) []Answer { return a[1:] },
		"answered twice": func(a []Answer) []Answer { return append(a, a[2]) },
		"wrong type": func(a []Answer) []Answer {
			a[2].Type = TypeScore
			return a
		},
		"noul no probability": func(a []Answer) []Answer {
			a[2].Probability = nil
			return a
		},
		"noul above one": func(a []Answer) []Answer {
			a[2].Probability = ptr(1.2)
			return a
		},
		"noul NaN": func(a []Answer) []Answer {
			a[2].Probability = ptr(math.NaN())
			return a
		},
		"choice not offered": func(a []Answer) []Answer {
			a[1].Choice = "Receipts"
			return a
		},
		"choice without its probability": func(a []Answer) []Answer {
			a[1].Probabilities = map[string]float64{"Contracts": 0.1}
			return a
		},
		"choice negative probability": func(a []Answer) []Answer {
			a[1].Probabilities["Contracts"] = -0.1
			return a
		},
		"score missing": func(a []Answer) []Answer {
			a[0].Score = nil
			return a
		},
		"score above top level": func(a []Answer) []Answer {
			a[0].Score = ptr(3)
			return a
		},
		"score below zero": func(a []Answer) []Answer {
			a[0].Score = ptr(-0.5)
			return a
		},
		"score level probability": func(a []Answer) []Answer {
			a[0].Probabilities["1"] = 2
			return a
		},
		"confidence above one": func(a []Answer) []Answer {
			a[1].Confidence = ptr(1.5)
			return a
		},
	}
	for name, m := range mutate {
		resp := Response{Answers: m(goodAnswers())}
		err := Normalize(testRequest(t), &resp)
		var ia *InvalidAnswerError
		if !errors.As(err, &ia) || ia.Code() != "txco_decide_invalid_answer" {
			t.Errorf("%s: err = %v, want *InvalidAnswerError", name, err)
		}
	}
}
