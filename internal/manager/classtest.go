package manager

import (
	"strings"
)

// truth is the result of a class test as far as Kea Roost can tell: a test can
// look at the packet of the client, which Kea Roost never sees.
type truth int8

const (
	unknown truth = iota
	yes
	no
)

func truthOf(b bool) truth {
	if b {
		return yes
	}
	return no
}

func (t truth) not() truth {
	switch t {
	case yes:
		return no
	case no:
		return yes
	}
	return unknown
}

func (t truth) and(o truth) truth {
	switch {
	case t == no || o == no:
		return no
	case t == yes && o == yes:
		return yes
	}
	return unknown
}

func (t truth) or(o truth) truth {
	switch {
	case t == yes || o == yes:
		return yes
	case t == no && o == no:
		return no
	}
	return unknown
}

// maxTestDepth bounds the nesting of a test, so that a long chain of brackets
// cannot exhaust the stack.
const maxTestDepth = 32

// evalTest evaluates a test made only of member('name'), not, and, or and
// brackets, as Kea's expression language writes them. member tells whether the
// client is in a class. Every other test is unknown.
func evalTest(test string, member func(string) truth) truth {
	p := testParser{tokens: testTokens(test), member: member}
	if p.tokens == nil {
		return unknown
	}
	t := p.or(0)
	if p.failed || p.pos != len(p.tokens) {
		return unknown
	}
	return t
}

type testParser struct {
	tokens []string
	pos    int
	member func(string) truth
	failed bool
}

func (p *testParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *testParser) or(depth int) truth {
	t := p.and(depth)
	for !p.failed && p.peek() == "or" {
		p.pos++
		t = t.or(p.and(depth))
	}
	return t
}

func (p *testParser) and(depth int) truth {
	t := p.unary(depth)
	for !p.failed && p.peek() == "and" {
		p.pos++
		t = t.and(p.unary(depth))
	}
	return t
}

func (p *testParser) unary(depth int) truth {
	if depth > maxTestDepth {
		p.failed = true
		return unknown
	}
	switch token := p.peek(); {
	case token == "not":
		p.pos++
		return p.unary(depth + 1).not()
	case token == "(":
		p.pos++
		t := p.or(depth + 1)
		if p.peek() != ")" {
			p.failed = true
			return unknown
		}
		p.pos++
		return t
	case strings.HasPrefix(token, "member:"):
		p.pos++
		return p.member(strings.TrimPrefix(token, "member:"))
	}
	p.failed = true
	return unknown
}

// testTokens splits a test into not, and, or, brackets and member:name. It
// returns nil for anything else.
func testTokens(test string) []string {
	var out []string
	rest := strings.TrimSpace(test)
	for rest != "" {
		switch {
		case rest[0] == '(' || rest[0] == ')':
			out = append(out, rest[:1])
			rest = rest[1:]
		case strings.HasPrefix(rest, "member("):
			inner := strings.TrimSpace(rest[len("member("):])
			if !strings.HasPrefix(inner, "'") {
				return nil
			}
			end := strings.Index(inner[1:], "'")
			if end < 0 {
				return nil
			}
			name := inner[1 : end+1]
			after := strings.TrimSpace(inner[end+2:])
			if !strings.HasPrefix(after, ")") {
				return nil
			}
			out = append(out, "member:"+name)
			rest = after[1:]
		default:
			word := rest
			if i := strings.IndexAny(rest, " \t\n()"); i >= 0 {
				word = rest[:i]
			}
			if word != "not" && word != "and" && word != "or" {
				return nil
			}
			out = append(out, word)
			rest = rest[len(word):]
		}
		rest = strings.TrimSpace(rest)
	}
	return out
}
