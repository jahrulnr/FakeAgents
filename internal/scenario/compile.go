package scenario

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/jsonpath"
)

// Compiled is a validated, indexed scenario.
type Compiled struct {
	Spec    Spec
	Type    string
	Order   int
	enabled bool

	hasCriteria   bool
	modelPatterns []*regexp.Regexp
	userRegex     *regexp.Regexp
	systemRegex   *regexp.Regexp
	headerRegex   map[string]*regexp.Regexp
	jsonRegex     map[string]*regexp.Regexp
}

// Name returns the scenario name.
func (c *Compiled) Name() string { return c.Spec.Name }

// Enabled reports whether the scenario is active.
func (c *Compiled) Enabled() bool { return c.enabled }

// WithRespond returns a shallow copy of the compiled scenario with a replaced
// response. It is used to activate a script step without recompiling matchers.
func (c *Compiled) WithRespond(respond Respond) *Compiled {
	copy := *c
	copy.Spec.Respond = respond
	copy.Type = respond.EffectiveType()
	return &copy
}

// HasMatchCriteria reports whether the scenario can be selected automatically.
// Scenarios with no criteria are opt-in and must be requested by name or
// trigger.
func (c *Compiled) HasMatchCriteria() bool { return c.hasCriteria }

// Set is an immutable collection of compiled scenarios.
type Set struct {
	items     []*Compiled
	byName    map[string]*Compiled
	byTrigger map[string]*Compiled
}

// Compile validates and indexes scenarios. Scenarios are matched by descending
// priority, then by their original order in configuration.
func Compile(specs []Spec) (*Set, error) {
	set := &Set{
		byName:    make(map[string]*Compiled, len(specs)),
		byTrigger: make(map[string]*Compiled, len(specs)),
	}
	seen := make(map[string]string)
	for index, spec := range specs {
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		key := strings.ToLower(spec.Name)
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("scenario name %q is already used by %q", spec.Name, previous)
		}
		seen[key] = spec.Name

		compiled := &Compiled{
			Spec:        spec,
			Type:        spec.Respond.EffectiveType(),
			Order:       index,
			enabled:     spec.Enabled == nil || *spec.Enabled,
			headerRegex: make(map[string]*regexp.Regexp, len(spec.Match.HeaderRegex)),
			jsonRegex:   make(map[string]*regexp.Regexp, len(spec.Match.JSONRegex)),
		}
		compiled.hasCriteria = hasMatchCriteria(spec.Match)
		for _, pattern := range spec.Match.Models {
			expr, err := globToRegexp(pattern)
			if err != nil {
				return nil, fmt.Errorf("scenario %q: invalid model pattern %q: %w", spec.Name, pattern, err)
			}
			compiled.modelPatterns = append(compiled.modelPatterns, expr)
		}
		if spec.Match.UserRegex != "" {
			expr, err := regexp.Compile(spec.Match.UserRegex)
			if err != nil {
				return nil, fmt.Errorf("scenario %q: invalid user_regex: %w", spec.Name, err)
			}
			compiled.userRegex = expr
		}
		if spec.Match.SystemRegex != "" {
			expr, err := regexp.Compile(spec.Match.SystemRegex)
			if err != nil {
				return nil, fmt.Errorf("scenario %q: invalid system_regex: %w", spec.Name, err)
			}
			compiled.systemRegex = expr
		}
		for header, pattern := range spec.Match.HeaderRegex {
			expr, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("scenario %q: invalid header_regex[%s]: %w", spec.Name, header, err)
			}
			compiled.headerRegex[strings.ToLower(header)] = expr
		}
		for path, pattern := range spec.Match.JSONRegex {
			expr, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("scenario %q: invalid json_regex[%s]: %w", spec.Name, path, err)
			}
			compiled.jsonRegex[path] = expr
		}
		set.items = append(set.items, compiled)
		set.byName[key] = compiled
		set.byTrigger[key] = compiled
		for _, trigger := range spec.Match.Triggers {
			trigger = strings.ToLower(strings.TrimSpace(trigger))
			if trigger == "" {
				continue
			}
			if previous, exists := set.byTrigger[trigger]; exists && previous != compiled {
				return nil, fmt.Errorf("scenario trigger %q is already used by %q", trigger, previous.Name())
			}
			set.byTrigger[trigger] = compiled
		}
	}
	sort.SliceStable(set.items, func(i, j int) bool {
		if set.items[i].Spec.Priority != set.items[j].Spec.Priority {
			return set.items[i].Spec.Priority > set.items[j].Spec.Priority
		}
		return set.items[i].Order < set.items[j].Order
	})
	return set, nil
}

// Lookup finds a scenario by name or trigger alias (case-insensitive).
func (s *Set) Lookup(name string) (*Compiled, bool) {
	if s == nil {
		return nil, false
	}
	compiled, ok := s.byTrigger[strings.ToLower(strings.TrimSpace(name))]
	if !ok || !compiled.enabled {
		return nil, false
	}
	return compiled, true
}

// Match returns the first enabled scenario whose criteria pass.
func (s *Set) Match(ctx *Context) (*Compiled, bool) {
	if s == nil || ctx == nil {
		return nil, false
	}
	for _, compiled := range s.items {
		if compiled.matches(ctx) {
			return compiled, true
		}
	}
	return nil, false
}

// All returns scenarios in matching order.
func (s *Set) All() []*Compiled {
	if s == nil {
		return nil
	}
	out := make([]*Compiled, len(s.items))
	copy(out, s.items)
	return out
}

// Names returns all scenario names sorted alphabetically.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.byName))
	for _, item := range s.byName {
		names = append(names, item.Name())
	}
	sort.Strings(names)
	return names
}

func (c *Compiled) matches(ctx *Context) bool {
	if !c.enabled || !c.hasCriteria {
		return false
	}
	m := c.Spec.Match
	if !matchList(m.Providers, ctx.Provider) {
		return false
	}
	if !matchList(m.Endpoints, ctx.Endpoint) {
		return false
	}
	if len(c.modelPatterns) > 0 {
		matched := false
		for _, pattern := range c.modelPatterns {
			if pattern.MatchString(ctx.Model) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if !matchList(m.Roles, ctx.LastRole) {
		return false
	}
	if m.Stream != nil && *m.Stream != ctx.Stream {
		return false
	}
	if m.Thinking != nil && *m.Thinking != ctx.ThinkingRequested {
		return false
	}
	if m.HasTools != nil && *m.HasTools != ctx.HasTools {
		return false
	}
	if m.HasToolResults != nil && *m.HasToolResults != ctx.HasToolResults {
		return false
	}
	if m.Turn != nil && *m.Turn != ctx.Turn {
		return false
	}
	for name, expected := range m.Headers {
		if !strings.EqualFold(ctx.Headers.Get(name), expected) {
			return false
		}
	}
	for name, expr := range c.headerRegex {
		if !expr.MatchString(ctx.Headers.Get(name)) {
			return false
		}
	}
	for _, name := range m.HeaderPresent {
		if strings.TrimSpace(ctx.Headers.Get(name)) == "" {
			return false
		}
	}
	for path, expected := range m.JSONEquals {
		actual, ok := jsonpath.Lookup(ctx.Raw, path)
		if !ok || !jsonpath.Equal(actual, expected) {
			return false
		}
	}
	for path, expr := range c.jsonRegex {
		actual, ok := jsonpath.Lookup(ctx.Raw, path)
		if !ok || !expr.MatchString(fmt.Sprint(actual)) {
			return false
		}
	}
	for _, path := range m.JSONExists {
		if _, ok := jsonpath.Lookup(ctx.Raw, path); !ok {
			return false
		}
	}
	for _, rule := range m.JSONCount {
		count := 0
		for _, value := range jsonpath.Select(ctx.Raw, rule.Path) {
			if matchesJSONCount(value, rule) {
				count++
			}
		}
		if rule.Min != nil && count < *rule.Min || rule.Max != nil && count > *rule.Max {
			return false
		}
	}
	if c.userRegex != nil && !c.userRegex.MatchString(ctx.LastUserMessage) {
		return false
	}
	if c.systemRegex != nil && !c.systemRegex.MatchString(ctx.System) {
		return false
	}
	if m.Probability > 0 && m.Probability < 1 {
		rng := ctx.Random
		if rng == nil {
			rng = rand.New(rand.NewPCG(uint64(ctx.Turn)+1, uint64(len(ctx.Session))+7))
		}
		if rng.Float64() >= m.Probability {
			return false
		}
	}
	return true
}

func hasMatchCriteria(m Match) bool {
	return len(m.Providers) > 0 || len(m.Endpoints) > 0 || len(m.Models) > 0 ||
		len(m.Roles) > 0 || m.Stream != nil || m.Thinking != nil || m.HasTools != nil ||
		m.HasToolResults != nil || len(m.Headers) > 0 || len(m.HeaderRegex) > 0 ||
		len(m.HeaderPresent) > 0 || len(m.JSONEquals) > 0 || len(m.JSONRegex) > 0 ||
		len(m.JSONExists) > 0 || len(m.JSONCount) > 0 || m.UserRegex != "" || m.SystemRegex != "" ||
		m.Turn != nil || m.Probability > 0
}

func matchesJSONCount(value any, rule JSONCount) bool {
	for path, expected := range rule.Where {
		actual, ok := jsonpath.Lookup(value, path)
		if !ok || !jsonpath.Equal(actual, expected) {
			return false
		}
	}
	for _, path := range rule.Exists {
		if _, ok := jsonpath.Lookup(value, path); !ok {
			return false
		}
	}
	for _, path := range rule.Empty {
		actual, ok := jsonpath.Lookup(value, path)
		if ok && actual != nil {
			if text, ok := actual.(string); !ok || text != "" {
				return false
			}
		}
	}
	return true
}

func matchList(allowed []string, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?i)^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
