package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

var className = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Kea defines these classes itself.
var builtinClasses = []string{"ALL", "KNOWN", "UNKNOWN", "BOOTP", "DROP"}

// ClassView is a client class prepared for display.
type ClassView struct {
	Name         string              `json:"name"`
	Test         string              `json:"test,omitempty"`
	Additional   bool                `json:"additional"`
	Options      []ReservationOption `json:"options"`
	Reservations int                 `json:"reservations"`
	// Subnets are the IDs of the subnets that list the class as additional.
	Subnets []int64 `json:"subnets"`
}

// ClassList is the answer of Classes.
type ClassList struct {
	Classes  []ClassView `json:"classes"`
	Editable bool        `json:"editable"`
}

// ClassInput is the editable part of a client class.
type ClassInput struct {
	Name    string              `json:"name"`
	Options []ReservationOption `json:"options"`
	// Test is Kea's expression for the class; empty assigns it only by reservation
	// or, for an additional class, wherever it is listed.
	// Nil keeps what the class has, so that a request with only options leaves both.
	Test       *string `json:"test"`
	Additional *bool   `json:"additional"`
}

// fill sets what the request leaves out from the existing entry, or from the
// defaults for a new class.
func (in *ClassInput) fill(entry map[string]any) {
	if in.Test == nil {
		test, _ := entry["test"].(string)
		in.Test = &test
	}
	if in.Additional == nil {
		additional := entry["only-in-additional-list"] == true || entry["only-if-required"] == true
		in.Additional = &additional
	}
}

// maxTestLength bounds a class test; Kea checks its syntax.
const maxTestLength = 1024

// Classes lists the client classes and how many reservations use them.
func (m *Manager) Classes(ctx context.Context) (*ClassList, error) {
	src := m.source(ctx).client
	config, err := src.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	classes, err := kea.ParseClasses(config)
	if err != nil {
		return nil, err
	}
	subnets, err := kea.ParseSubnets(config)
	if err != nil {
		return nil, err
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, err
	}
	commands, err := src.Commands(ctx)
	if err != nil {
		return nil, err
	}
	list := &ClassList{Classes: []ClassView{}, Editable: slices.Contains(commands, "class-update")}
	for _, c := range classes {
		v := ClassView{Name: c.Name, Test: c.Test, Additional: c.Additional, Options: []ReservationOption{}, Subnets: subnetsListing(subnets, c.Name)}
		for _, o := range c.Options {
			v.Options = append(v.Options, ReservationOption{Name: o.Name, Code: o.Code, Data: o.Data})
		}
		for _, h := range hosts {
			if slices.Contains(h.Classes(), c.Name) {
				v.Reservations++
			}
		}
		list.Classes = append(list.Classes, v)
	}
	return list, nil
}

// subnetsListing returns the subnets that list a class as additional.
func subnetsListing(subnets []kea.Subnet, name string) []int64 {
	out := []int64{}
	for _, s := range subnets {
		if slices.Contains(s.AdditionalClasses, name) {
			out = append(out, s.ID)
		}
	}
	return out
}

func checkClass(in *ClassInput) []string {
	var problems []string
	if len(in.Options) == 0 {
		problems = append(problems, "a class needs at least one option")
	}
	for _, o := range in.Options {
		if strings.TrimSpace(o.Name) == "" && o.Code == 0 {
			problems = append(problems, "DHCP option without a name")
		}
	}
	*in.Test = strings.TrimSpace(*in.Test)
	if len(*in.Test) > maxTestLength {
		problems = append(problems, fmt.Sprintf("the test is longer than %d characters", maxTestLength))
	}
	if strings.ContainsFunc(*in.Test, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		problems = append(problems, "the test must be a single line")
	}
	return problems
}

// classEntry builds the option list, test and flag of a class and keeps settings
// the form does not show, such as csv-format. The flag is written under its Kea
// 3.0 name; the old one is dropped.
func classEntry(entry map[string]any, in *ClassInput) {
	previous, _ := entry["option-data"].([]any)
	known := make([]map[string]any, 0, len(previous))
	for _, item := range previous {
		if o, ok := item.(map[string]any); ok {
			known = append(known, o)
		}
	}
	entry["option-data"] = mergeOptions(known, in.Options)
	if *in.Test != "" {
		entry["test"] = *in.Test
	} else {
		delete(entry, "test")
	}
	delete(entry, "only-if-required")
	if *in.Additional {
		entry["only-in-additional-list"] = true
	} else {
		delete(entry, "only-in-additional-list")
	}
}

// CreateClass adds a client class.
func (m *Manager) CreateClass(ctx context.Context, user string, in ClassInput) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	in.fill(map[string]any{})
	problems := checkClass(&in)
	if !className.MatchString(in.Name) {
		problems = append(problems, fmt.Sprintf("invalid class name %q", in.Name))
	}
	if slices.Contains(builtinClasses, in.Name) {
		problems = append(problems, fmt.Sprintf("%s is a built-in class of Kea", in.Name))
	}
	existing, err := m.kea.ClassGet(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		problems = append(problems, fmt.Sprintf("class %s already exists", in.Name))
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return m.run(ctx, change{
		user:         user,
		action:       "class-add",
		target:       in.Name,
		backupReason: "class-add",
		mutate: func(ctx context.Context, k *kea.Client) error {
			entry := map[string]any{"name": in.Name}
			classEntry(entry, &in)
			return k.ClassAdd(ctx, entry)
		},
		verify: func(ctx context.Context, k *kea.Client) error { return verifyClass(ctx, k, &in) },
	}), nil
}

func verifyClass(ctx context.Context, k *kea.Client, in *ClassInput) error {
	entry, err := k.ClassGet(ctx, in.Name)
	if err != nil {
		return err
	}
	if entry == nil {
		return errors.New("class is missing in Kea")
	}
	test, _ := entry["test"].(string)
	if test != *in.Test {
		return fmt.Errorf("the test is %q instead of %q", test, *in.Test)
	}
	additional := entry["only-in-additional-list"] == true || entry["only-if-required"] == true
	if additional != *in.Additional {
		return fmt.Errorf("only-in-additional-list is %v instead of %v", additional, *in.Additional)
	}
	return nil
}

// UpdateClass changes the options of a class. class-update overwrites the whole
// entry, so it is sent complete; Kea does not allow renaming.
func (m *Manager) UpdateClass(ctx context.Context, user, name string, in ClassInput) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	in.Name = name
	entry, err := m.kea.ClassGet(ctx, name)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, ErrNotFound
	}
	in.fill(entry)
	if problems := checkClass(&in); len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return m.run(ctx, change{
		user:         user,
		action:       "class-update",
		target:       name,
		backupReason: "class-update",
		mutate: func(ctx context.Context, k *kea.Client) error {
			classEntry(entry, &in)
			return k.ClassUpdate(ctx, entry)
		},
		verify: func(ctx context.Context, k *kea.Client) error { return verifyClass(ctx, k, &in) },
	}), nil
}

// DeleteClass removes a class no reservation uses.
func (m *Manager) DeleteClass(ctx context.Context, user, name string) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	entry, err := m.kea.ClassGet(ctx, name)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, ErrNotFound
	}
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, err
	}
	used := 0
	for _, h := range hosts {
		if slices.Contains(h.Classes(), name) {
			used++
		}
	}
	var problems []string
	if used > 0 {
		problems = append(problems, fmt.Sprintf("class %s is used by %d reservations", name, used))
	}
	config, err := m.kea.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, err
	}
	if n := countListings(doc, name); n > 0 {
		problems = append(problems, fmt.Sprintf("class %s is listed as additional class by %d pools, subnets or shared networks", name, n))
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return m.run(ctx, change{
		user:         user,
		action:       "class-del",
		target:       name,
		backupReason: "class-del",
		mutate:       func(ctx context.Context, k *kea.Client) error { return k.ClassDel(ctx, name) },
		verify: func(ctx context.Context, k *kea.Client) error {
			entry, err := k.ClassGet(ctx, name)
			if err != nil {
				return err
			}
			if entry != nil {
				return errors.New("class still exists")
			}
			return nil
		},
	}), nil
}

// countListings counts the pools, subnets and shared networks that list a class
// as additional, under the Kea 3.0 name or the old one.
func countListings(node any, name string) int {
	n := 0
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "evaluate-additional-classes" || key == "require-client-classes" {
				if slices.Contains(stringList(child), name) {
					n++
				}
				continue
			}
			n += countListings(child, name)
		}
	case []any:
		for _, child := range v {
			n += countListings(child, name)
		}
	}
	return n
}
