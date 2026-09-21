package decide

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// questionKeyRE bounds question keys to names a txcl path can address:
// `._x.answers.<key>.probability`. A key with a dot or a space would come
// back under a path no WHEN clause can reach.
var questionKeyRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

// scoreEpsilon absorbs float rounding at the ends of the score scale.
const scoreEpsilon = 1e-6

// ParseState checks that raw (a JSON value) is usable as decision state: a
// non-empty string, an object, or an array. It returns the value unchanged.
func ParseState(raw gjson.Result) (json.RawMessage, error) {
	switch {
	case !raw.Exists() || raw.Type == gjson.Null:
		return nil, &InvalidWithError{Reason: "WITH state is required"}
	case raw.Type == gjson.String:
		if strings.TrimSpace(raw.String()) == "" {
			return nil, &InvalidWithError{Reason: "WITH state is an empty string"}
		}
	case raw.IsObject(), raw.IsArray():
	default:
		return nil, &InvalidWithError{Reason: "WITH state must be a string, an object, or an array"}
	}
	return json.RawMessage(raw.Raw), nil
}

// ParseQuestions decodes and validates the WITH `questions` object:
//
//	{ "<key>": { "type": "noul|choice|score", "instructions": "…", "criteria": … } }
//
// criteria by type:
//
//	noul    optional; an object whose keys are only "true" and/or "false"
//	choice  required; an object of option name → description (≥ 1 option)
//	score   required; an array of level labels, lowest first (≥ 2 levels)
//
// Author order is preserved. Unknown question fields are refused so a typo
// (`options` for `criteria`) fails loudly instead of being dropped.
// Provider-specific limits (e.g. a maximum number of choices) are the
// backend's to enforce before it sends anything.
func ParseQuestions(raw gjson.Result) ([]Question, error) {
	if !raw.Exists() || raw.Type == gjson.Null {
		return nil, &InvalidWithError{Reason: "WITH questions is required"}
	}
	if !raw.IsObject() {
		return nil, &InvalidWithError{Reason: "WITH questions must be an object keyed by question id"}
	}

	var (
		out  []Question
		seen = map[string]bool{}
		err  error
	)
	raw.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		if !questionKeyRE.MatchString(key) {
			err = &InvalidWithError{Reason: fmt.Sprintf("question key %q must be 1-64 of [A-Za-z0-9_-], not starting with '-'", key)}
			return false
		}
		if seen[key] {
			err = &InvalidWithError{Reason: fmt.Sprintf("question key %q appears twice", key)}
			return false
		}
		seen[key] = true

		var q Question
		q, err = parseQuestion(key, v)
		if err != nil {
			return false
		}
		out = append(out, q)
		return true
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, &InvalidWithError{Reason: "WITH questions is empty"}
	}
	return out, nil
}

func parseQuestion(key string, v gjson.Result) (Question, error) {
	bad := func(format string, a ...any) error {
		return &InvalidWithError{Reason: fmt.Sprintf("question %q: ", key) + fmt.Sprintf(format, a...)}
	}
	if !v.IsObject() {
		return Question{}, bad("must be an object with type, instructions, criteria")
	}

	var unknown string
	v.ForEach(func(f, _ gjson.Result) bool {
		switch f.String() {
		case "type", "instructions", "criteria":
			return true
		}
		unknown = f.String()
		return false
	})
	if unknown != "" {
		return Question{}, bad("unknown field %q (allowed: type, instructions, criteria)", unknown)
	}

	q := Question{Key: key, Type: v.Get("type").String()}
	instr := v.Get("instructions")
	if instr.Type != gjson.String || strings.TrimSpace(instr.String()) == "" {
		return Question{}, bad("instructions must be a non-empty string")
	}
	q.Instructions = instr.String()
	criteria := v.Get("criteria")

	switch q.Type {
	case TypeNoul:
		if !criteria.Exists() {
			break
		}
		if !criteria.IsObject() {
			return Question{}, bad("noul criteria must be an object with keys true and/or false")
		}
		var err error
		criteria.ForEach(func(ck, cv gjson.Result) bool {
			if cv.Type != gjson.String {
				err = bad("noul criteria %q must be a string", ck.String())
				return false
			}
			switch ck.String() {
			case "true":
				q.True = cv.String()
			case "false":
				q.False = cv.String()
			default:
				err = bad("noul criteria may only describe true and false, not %q", ck.String())
				return false
			}
			return true
		})
		if err != nil {
			return Question{}, err
		}

	case TypeChoice:
		if !criteria.IsObject() {
			return Question{}, bad("choice criteria must be an object of option name → description")
		}
		var err error
		names := map[string]bool{}
		criteria.ForEach(func(ck, cv gjson.Result) bool {
			name := ck.String()
			if strings.TrimSpace(name) == "" {
				err = bad("choice option names must be non-empty")
				return false
			}
			if names[name] {
				err = bad("choice option %q appears twice", name)
				return false
			}
			if cv.Type != gjson.String {
				err = bad("choice option %q: description must be a string", name)
				return false
			}
			names[name] = true
			q.Choices = append(q.Choices, Option{Name: name, Description: cv.String()})
			return true
		})
		if err != nil {
			return Question{}, err
		}
		if len(q.Choices) == 0 {
			return Question{}, bad("choice criteria must offer at least one option")
		}

	case TypeScore:
		if !criteria.IsArray() {
			return Question{}, bad("score criteria must be an array of levels, lowest first")
		}
		for i, lv := range criteria.Array() {
			if lv.Type != gjson.String || strings.TrimSpace(lv.String()) == "" {
				return Question{}, bad("score level %d must be a non-empty string", i)
			}
			q.Levels = append(q.Levels, lv.String())
		}
		if len(q.Levels) < 2 {
			return Question{}, bad("score criteria must have at least two levels")
		}

	case "":
		return Question{}, bad("type is required (noul, choice, or score)")
	default:
		return Question{}, bad("unknown type %q (noul, choice, or score)", q.Type)
	}
	return q, nil
}

// Normalize enforces the answer contract on a backend's response, in place:
//
//   - every question has exactly one answer, of the type it was asked as;
//   - a choice is one of the offered names, and its Probability is the
//     distribution's value for that name;
//   - a score lies on the level-index scale, 0 … len(Levels)-1;
//   - every probability, and a reported confidence, is a finite number in
//     [0, 1];
//   - distributions keep only offered names / level indices;
//   - answers come back in question order, and nothing else survives.
//
// Any violation fails the whole call: a stack never sees a partial answer
// set, because a missing path reads as zero in a WHEN comparison.
func Normalize(req Request, resp *Response) error {
	byKey := make(map[string]Answer, len(resp.Answers))
	for _, a := range resp.Answers {
		if _, dup := byKey[a.Key]; dup {
			return &InvalidAnswerError{Question: a.Key, Reason: "answered twice"}
		}
		byKey[a.Key] = a
	}

	out := make([]Answer, 0, len(req.Questions))
	for _, q := range req.Questions {
		a, ok := byKey[q.Key]
		if !ok {
			return &InvalidAnswerError{Question: q.Key, Reason: "no answer"}
		}
		if a.Type != q.Type {
			return &InvalidAnswerError{Question: q.Key, Reason: fmt.Sprintf("answered as %q, asked as %q", a.Type, q.Type)}
		}
		n, err := normalizeAnswer(q, a)
		if err != nil {
			return err
		}
		out = append(out, n)
	}
	resp.Answers = out
	return nil
}

func normalizeAnswer(q Question, a Answer) (Answer, error) {
	bad := func(format string, args ...any) error {
		return &InvalidAnswerError{Question: q.Key, Reason: fmt.Sprintf(format, args...)}
	}
	n := Answer{Key: q.Key, Type: q.Type}

	switch q.Type {
	case TypeNoul:
		if a.Probability == nil {
			return Answer{}, bad("no probability")
		}
		if !isProbability(*a.Probability) {
			return Answer{}, bad("probability %v is not in [0, 1]", *a.Probability)
		}
		p := *a.Probability
		n.Probability = &p

	case TypeChoice:
		offered := false
		for _, o := range q.Choices {
			if o.Name == a.Choice {
				offered = true
				break
			}
		}
		if !offered {
			return Answer{}, bad("choice %q was not offered", a.Choice)
		}
		dist := map[string]float64{}
		for _, o := range q.Choices {
			if p, ok := a.Probabilities[o.Name]; ok {
				if !isProbability(p) {
					return Answer{}, bad("probability %v for %q is not in [0, 1]", p, o.Name)
				}
				dist[o.Name] = p
			}
		}
		p, ok := dist[a.Choice]
		if !ok {
			return Answer{}, bad("no probability for the chosen %q", a.Choice)
		}
		n.Choice = a.Choice
		n.Probability = &p
		n.Probabilities = dist

	case TypeScore:
		if a.Score == nil {
			return Answer{}, bad("no score")
		}
		s := *a.Score
		top := float64(len(q.Levels) - 1)
		if math.IsNaN(s) || s < -scoreEpsilon || s > top+scoreEpsilon {
			return Answer{}, bad("score %v is outside 0 … %v (level index scale)", s, top)
		}
		dist := map[string]float64{}
		for i := range q.Levels {
			k := strconv.Itoa(i)
			if p, ok := a.Probabilities[k]; ok {
				if !isProbability(p) {
					return Answer{}, bad("probability %v for level %s is not in [0, 1]", p, k)
				}
				dist[k] = p
			}
		}
		n.Score = &s
		n.Probabilities = dist
	}
	if a.Confidence != nil {
		if !isProbability(*a.Confidence) {
			return Answer{}, bad("confidence %v is not in [0, 1]", *a.Confidence)
		}
		c := *a.Confidence
		n.Confidence = &c
	}
	return n, nil
}

func isProbability(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1
}
