package typewirego

// Token identifies a wire inside a container. Tokens compare by identity:
// two wires built with the same label are two tokens. The zero Token is never
// bound.
type Token struct {
	p *tokenData
}

type tokenData struct {
	label string
}

func newToken(label string) Token {
	return Token{p: &tokenData{label: label}}
}

// Label returns the diagnostic label the token was created with.
func (t Token) Label() string {
	if t.p == nil {
		return ""
	}
	return t.p.label
}

func (t Token) isZero() bool { return t.p == nil }

// Scope says how often a wire's creator runs within one container.
type Scope string

const (
	// Singleton wires run their creator once per container and cache the value.
	Singleton Scope = "singleton"
	// Transient wires run their creator on every resolve and cache nothing.
	Transient Scope = "transient"
)
