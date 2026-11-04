// Package agents is the core of `wwz-load agents` (spec 015, "Contrato de
// agents e top"; ADR 0036): N concurrent agents, each running multi-turn
// sessions against the target. Every tool call received is executed by
// internal/tools (in process or on a wwz-tools server, ADR 0034) and its
// result goes back as a tool result; requests that fail with a transient
// class are retried with full-jitter backoff honouring retry-after. It reuses
// the client, the SSE parser, the error classes and the histograms of
// internal/loadgen.
package agents

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// Profile is the --profile of the agents.
type Profile string

// Profiles. Mixed assigns coder, pesquisa and chat in rotation by the agent
// index.
const (
	Coder    Profile = "coder"
	Pesquisa Profile = "pesquisa"
	Chat     Profile = "chat"
	Mixed    Profile = "mixed"
)

// ParseProfile validates a --profile value.
func ParseProfile(s string) (Profile, error) {
	switch p := Profile(s); p {
	case Coder, Pesquisa, Chat, Mixed:
		return p, nil
	}
	return "", fmt.Errorf("profile %q: want coder, pesquisa, chat or mixed", s)
}

var rotation = [...]Profile{Coder, Pesquisa, Chat}

// For returns the profile of agent a (mixed rotates).
func (p Profile) For(a int) Profile {
	if p == Mixed {
		return rotation[a%len(rotation)]
	}
	return p
}

// Reasons of a session that failed besides the error class of the request
// that failed it (spec 015, "Sessão").
const (
	// ReasonMaxToolRounds: --max-tool-rounds requests in a turn and the
	// last one still asked for tools.
	ReasonMaxToolRounds = "max_tool_rounds"
	// ReasonTools: the tools server could not run a call (unreachable,
	// unauthorized or an unexpected status).
	ReasonTools = "tools_unavailable"
)

// script is the fixed text of a profile.
type script struct {
	system string
	first  string
	next   []string
}

var scripts = map[Profile]script{
	Coder: {
		system: "Você é um agente de programação num repositório Go. Use as ferramentas para ler, buscar, editar e testar o código antes de responder.",
		first:  "O teste de internal/cache falha de forma intermitente. Encontre a causa e corrija.",
		next: []string{
			"Rode os testes de novo e confirme que passam.",
			"Agora adicione um teste que cubra o caso que falhava.",
			"Revise o diff e explique a mudança em poucas linhas.",
			"Procure outros lugares do código com o mesmo problema.",
			"Atualize a documentação do pacote para refletir a correção.",
		},
	},
	Pesquisa: {
		system: "Você é um agente de pesquisa. Busque na web, leia as fontes e cite-as na resposta.",
		first:  "Quais são as diferenças entre os limites de taxa da OpenAI e da Anthropic?",
		next: []string{
			"Encontre uma fonte mais recente e compare com a anterior.",
			"Resuma o que muda para quem roteia tráfego entre os dois provedores.",
			"Há algum limite por organização além do limite por chave?",
			"Liste as fontes consultadas com uma frase sobre cada uma.",
		},
	},
	Chat: {
		system: "Você é um assistente prestativo e conciso.",
		first:  "Me explique em poucas linhas o que é um roteador de modelos de linguagem.",
		next: []string{
			"Dê um exemplo concreto.",
			"E quais são os riscos de usar um?",
			"Resuma tudo numa frase.",
			"Como eu testaria um roteador desses sob carga?",
		},
	},
}

// declared are the tools of each profile in the loadgen form, built once
// from the wwz-tools catalogue (chat declares none).
var declared = func() map[Profile][]loadgen.Tool {
	m := map[Profile][]loadgen.Tool{}
	for _, p := range rotation {
		ts, _ := tools.Tools(tools.Profile(p)) // fixed, valid profiles
		for _, t := range ts {
			m[p] = append(m[p], loadgen.Tool{Name: t.Name, Description: t.Description, Schema: t.Schema})
		}
	}
	return m
}()

// Config is one `wwz-load agents` run.
type Config struct {
	// Template carries protocol, URL, key, model, stream, max tokens and
	// the extra headers; messages, tools and seed are set per request.
	Template      loadgen.Request
	Agents        int
	Profile       Profile
	Turns         int
	MaxToolRounds int
	// Sessions per agent when Duration is 0.
	Sessions int
	// Duration > 0: agents open sessions until it elapses.
	Duration      time.Duration
	ThinkTime     time.Duration
	SpawnInterval time.Duration
	// Seed is --seed: ≠ 0 sends x-wwz-seed: Seed + g and seeds the tools
	// session of g with Seed + g.
	Seed int64
	// DrawSeed is the effective seed of the run (Seed, or a random one): it
	// draws the continuation messages and the backoff jitter.
	DrawSeed int64
	// RunID is the <run> of x-wwz-conversation-id; "" = derived from Seed
	// when Seed ≠ 0 (reproducible), else random.
	RunID         string
	Retries       int
	Backoff       time.Duration
	BackoffMax    time.Duration
	RetryAfterMax time.Duration
	Tools         ToolRunner
	// Progress, when set, is called every ProgressEvery.
	Progress      func(Progress)
	ProgressEvery time.Duration
}

// Progress is a snapshot for the progress line.
type Progress struct {
	Elapsed                                        time.Duration
	SessionsStarted, SessionsEnded, SessionsFailed int64
	Requests, Errors, Retries                      int64
}

var errConfig = errors.New("agents config")

// Validate checks the ranges of the configuration.
func (c *Config) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", errConfig, fmt.Sprintf(format, a...))
	}
	if _, err := ParseProfile(string(c.Profile)); err != nil {
		return bad("%v", err)
	}
	switch {
	case c.Agents < 1:
		return bad("agents must be ≥ 1, got %d", c.Agents)
	case c.Turns < 1:
		return bad("turns must be ≥ 1, got %d", c.Turns)
	case c.MaxToolRounds < 1:
		return bad("max tool rounds must be ≥ 1, got %d", c.MaxToolRounds)
	case c.Sessions < 1:
		return bad("sessions must be ≥ 1, got %d", c.Sessions)
	case c.Duration < 0 || c.ThinkTime < 0 || c.SpawnInterval < 0:
		return bad("duration, think time and spawn interval must be ≥ 0")
	case c.Retries < 0:
		return bad("retries must be ≥ 0, got %d", c.Retries)
	case c.Backoff < 0 || c.BackoffMax < 0 || c.RetryAfterMax < 0:
		return bad("backoff, backoff max and retry-after max must be ≥ 0")
	case c.Tools == nil:
		return bad("no tool runner")
	case c.RunID != "" && strings.Trim(c.RunID, "0123456789abcdef") != "":
		return bad("run id %q: want lowercase hex", c.RunID)
	}
	return nil
}
