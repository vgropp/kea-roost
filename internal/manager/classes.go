package manager

import (
	"context"
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
	Options      []ReservationOption `json:"options"`
	Reservations int                 `json:"reservations"`
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
}

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
		v := ClassView{Name: c.Name, Test: c.Test, Options: []ReservationOption{}}
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

func checkClassOptions(options []ReservationOption) []string {
	var problems []string
	if len(options) == 0 {
		problems = append(problems, "a class needs at least one option")
	}
	for _, o := range options {
		if strings.TrimSpace(o.Name) == "" && o.Code == 0 {
			problems = append(problems, "DHCP option without a name")
		}
	}
	return problems
}

// classEntry builds the option list of a class and keeps settings the form does
// not show, such as csv-format.
func classEntry(entry map[string]any, in *ClassInput) {
	previous, _ := entry["option-data"].([]any)
	known := make([]map[string]any, 0, len(previous))
	for _, item := range previous {
		if o, ok := item.(map[string]any); ok {
			known = append(known, o)
		}
	}
	entry["option-data"] = mergeOptions(known, in.Options)
}

// CreateClass adds a client class.
func (m *Manager) CreateClass(ctx context.Context, user string, in ClassInput) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	problems := checkClassOptions(in.Options)
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
		verify: func(ctx context.Context, k *kea.Client) error { return verifyClass(ctx, k, in.Name) },
	}), nil
}

func verifyClass(ctx context.Context, k *kea.Client, name string) error {
	entry, err := k.ClassGet(ctx, name)
	if err != nil {
		return err
	}
	if entry == nil {
		return errors.New("class is missing in Kea")
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

	if problems := checkClassOptions(in.Options); len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	entry, err := m.kea.ClassGet(ctx, name)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, ErrNotFound
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
		verify: func(ctx context.Context, k *kea.Client) error { return verifyClass(ctx, k, name) },
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
	if used > 0 {
		return nil, &ValidationError{Problems: []string{fmt.Sprintf("class %s is used by %d reservations", name, used)}}
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
