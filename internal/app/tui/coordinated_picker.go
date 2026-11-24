package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/losinggeneration/tau/agent/session"
	"github.com/losinggeneration/tau/rpc/coord"
	tauui "github.com/losinggeneration/tau/tui"
)

// NewInstanceChoice is the picker value for starting a worker in the launch directory.
const NewInstanceChoice = "+"

// Instance picker actions reported in InstanceSelection.Action.
const (
	InstanceActionOpen            = "open"
	InstanceActionRestart         = "restart"
	InstanceActionRestartOutdated = "restart-outdated"
	InstanceActionStop            = "stop"
	InstanceActionRename          = "rename"
)

const (
	instancePickerRestart tauui.Action = iota + 300
	instancePickerRestartOutdated
	instancePickerStop
	instancePickerRename
)

// InstancePickerState keeps the filter and focused row between picker runs.
// FocusID wins when that row still exists; Focused is the fallback index.
type InstancePickerState struct {
	Filter  string
	Focused int
	FocusID string
}

// InstanceSelection is the picker outcome. The zero value means cancel.
// Value is the chosen row value (an instance ID or NewInstanceChoice).
type InstanceSelection struct {
	Action  string
	Targets []coord.Instance
	Value   string
}

// InstancePickerJob runs a confirmed stop or restart outside the picker. Finish
// applies the completed result on the UI loop and is called once, after Done.
type InstancePickerJob struct {
	Targets []coord.Instance
	Notice  string
	Done    <-chan struct{}
	Finish  func() (current, notice string, replacements map[string]string)
}

func (j *InstancePickerJob) Includes(id string) bool {
	return j != nil && slices.ContainsFunc(j.Targets, func(target coord.Instance) bool { return target.ID == id })
}

// SelectInstance picks a running worker or offers to start one in cwd.
// Idle workers are managed in the background. Busy workers are returned to
// the caller for confirmation before starting the job.
func SelectInstance(ctx context.Context, instances []coord.Instance, updates <-chan coord.Frame, cwd, current, notice string, state *InstancePickerState, job *InstancePickerJob, start func(InstanceSelection) *InstancePickerJob) (InstanceSelection, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if job != nil {
		notice = job.Notice
	}
	var (
		selection InstanceSelection
		previous  InstancePickerState
	)
	if state != nil {
		previous = *state
	}
	items := func(filter string) []tauui.DialogPickerItem {
		return instancePickerItems(pickerInstancesDuringJob(instances, job), cwd, current, filter)
	}
	watchJob := func(root *sessionSelectorRoot, pending *InstancePickerJob) {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-pending.Done:
			}
			if err := root.app.Post(func(ui *tauui.UpdateCtx) {
				focused, _ := root.picker.FocusedItem()
				var replacements map[string]string
				current, notice, replacements = pending.Finish()
				job = nil
				if replacement := replacements[focused.Value]; replacement != "" {
					focused.Value = replacement
				}
				root.picker.SetHeader(instancePickerHeader(notice))
				setPickerItems(root.picker, items(root.picker.Filter()), focused.Value)
				ui.InvalidateLayout()
				ui.InvalidateAll()
			}); err != nil {
				return // picker closed; the caller can still consume the result
			}
		}()
	}
	root, err := runTerminalSelector(ctx, terminalSelector{
		header: instancePickerHeader(notice),
		title:  "tau — select instance",
		filter: previous.Filter,
		items:  items,
		focus: func(items []tauui.DialogPickerItem) int {
			return instancePickerFocus(items, previous)
		},
		keys:      instancePickerKey,
		canSelect: func(item tauui.DialogPickerItem) bool { return !job.Includes(item.Value) },
		onAction: func(root *sessionSelectorRoot, act tauui.Action, ui *tauui.Ctx) bool {
			focused, _ := root.picker.FocusedItem()
			choice, handled := instancePickerAction(act, instances, focused.Value)
			if !handled || choice.Action == "" || job != nil {
				return handled
			}
			_, confirm := instanceManagementPrompt(choice.Action, choice.Targets)
			if confirm || choice.Action == InstanceActionRename {
				selection = choice
				root.app.Quit()
				return true
			}
			job = start(choice)
			root.picker.SetHeader(instancePickerHeader(job.Notice))
			setPickerItems(root.picker, items(root.picker.Filter()), focused.Value)
			ui.InvalidateLayout()
			ui.InvalidateAll()
			watchJob(root, job)
			return true
		},
		start: func(root *sessionSelectorRoot) func() {
			if job != nil {
				watchJob(root, job)
			}
			return watchPickerInstances(ctx, root, updates, func(frame coord.Frame) []tauui.DialogPickerItem {
				instances = updatePickerInstances(instances, frame)
				return items(root.picker.Filter())
			})
		},
	})
	if err != nil {
		return InstanceSelection{}, err
	}
	if state != nil {
		*state = InstancePickerState{Filter: root.picker.Filter(), Focused: root.picker.Focused()}
		if item, ok := root.picker.FocusedItem(); ok {
			state.FocusID = item.Value
		}
	}
	if selection.Action == "" && root.selected != "" {
		selection = InstanceSelection{Action: InstanceActionOpen, Value: root.selected}
		if instance, ok := findPickerInstance(instances, root.selected); ok {
			selection.Targets = []coord.Instance{instance}
		}
	}
	return selection, nil
}

func instancePickerHeader(notice string) string {
	header := "Tau instances — Enter open · F1 restart · Shift+F1 restart outdated · F2 stop · F3 rename · Esc cancel"
	if notice != "" {
		header += "\n" + sanitizeStatus(notice)
	}
	return header
}

func setPickerItems(picker *tauui.DialogPicker, items []tauui.DialogPickerItem, focusID string) {
	previous := InstancePickerState{Focused: picker.Focused(), FocusID: focusID}
	picker.SetItems(items)
	picker.SetFocused(instancePickerFocus(items, previous))
}

// Keep target rows in place until the job returns the replacement IDs.
func pickerInstancesDuringJob(instances []coord.Instance, job *InstancePickerJob) []coord.Instance {
	if job == nil {
		return instances
	}
	instances = slices.Clone(instances)
	for _, target := range job.Targets {
		target.PendingInputs = 0
		target.State = coord.StateWorking
		target.LinkState = coord.LinkConnected
		instances = updatePickerInstances(instances, coord.Frame{InstanceID: target.ID, Instance: &target})
	}
	return instances
}

func instancePickerKey(ks tauui.Keystroke) (tauui.Action, bool) {
	switch {
	case ks.Key == tauui.KeyF1 && ks.Mod == 0:
		return instancePickerRestart, true
	case ks.Key == tauui.KeyF1 && ks.Mod == tauui.ModShift:
		return instancePickerRestartOutdated, true
	case ks.Key == tauui.KeyF2 && ks.Mod == 0:
		return instancePickerStop, true
	case ks.Key == tauui.KeyF3 && ks.Mod == 0:
		return instancePickerRename, true
	}
	return tauui.ActionNone, false
}

// instancePickerAction maps a picker shortcut to a selection. handled reports
// whether act belongs to the instance picker; an empty Action means the key
// does nothing for the focused row.
func instancePickerAction(act tauui.Action, instances []coord.Instance, focused string) (selection InstanceSelection, handled bool) {
	switch act {
	case instancePickerRestart, instancePickerStop, instancePickerRename:
		instance, ok := findPickerInstance(instances, focused)
		if !ok {
			return InstanceSelection{}, true
		}
		action := InstanceActionRestart
		switch act {
		case instancePickerStop:
			action = InstanceActionStop
		case instancePickerRename:
			action = InstanceActionRename
		}
		return InstanceSelection{Action: action, Targets: []coord.Instance{instance}, Value: instance.ID}, true
	case instancePickerRestartOutdated:
		var targets []coord.Instance
		for _, instance := range instances {
			if instance.Outdated && pickerInstanceConnected(instance) {
				targets = append(targets, instance)
			}
		}
		if len(targets) == 0 {
			return InstanceSelection{}, true
		}
		return InstanceSelection{Action: InstanceActionRestartOutdated, Targets: targets}, true
	}
	return InstanceSelection{}, false
}

func findPickerInstance(instances []coord.Instance, id string) (coord.Instance, bool) {
	if id == "" || id == NewInstanceChoice {
		return coord.Instance{}, false
	}
	i := slices.IndexFunc(instances, func(instance coord.Instance) bool {
		return instance.ID == id && pickerInstanceConnected(instance)
	})
	if i < 0 {
		return coord.Instance{}, false
	}
	return instances[i], true
}

func pickerInstanceConnected(instance coord.Instance) bool {
	return instance.LinkState == "" || instance.LinkState == coord.LinkConnected
}

func instancePickerFocus(items []tauui.DialogPickerItem, state InstancePickerState) int {
	if state.FocusID != "" {
		if i := slices.IndexFunc(items, func(item tauui.DialogPickerItem) bool { return item.Value == state.FocusID }); i >= 0 {
			return i
		}
	}
	return max(0, min(state.Focused, len(items)-1))
}

func watchPickerInstances(ctx context.Context, root *sessionSelectorRoot, updates <-chan coord.Frame, apply func(coord.Frame) []tauui.DialogPickerItem) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var frame coord.Frame
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					root.app.Quit()
					return
				}
				frame = update
			}
			if frame.Type != coord.TypeInstanceUpdate {
				continue
			}
			applied := make(chan struct{})
			if err := root.app.Post(func(ui *tauui.UpdateCtx) {
				defer close(applied)
				focused, _ := root.picker.FocusedItem()
				setPickerItems(root.picker, apply(frame), focused.Value)
				ui.InvalidateAll()
			}); err != nil {
				return // picker is closing
			}
			select {
			case <-ctx.Done():
				return
			case <-applied:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func updatePickerInstances(instances []coord.Instance, frame coord.Frame) []coord.Instance {
	i, found := slices.BinarySearchFunc(instances, frame.InstanceID, func(inst coord.Instance, id string) int {
		return cmp.Compare(inst.ID, id)
	})
	if frame.Instance == nil {
		if found {
			return slices.Delete(instances, i, i+1)
		}
		return instances
	}
	if found {
		instances[i] = *frame.Instance
		return instances
	}
	return slices.Insert(instances, i, *frame.Instance)
}

func instancePickerItems(instances []coord.Instance, cwd, current, filter string) []tauui.DialogPickerItem {
	filter = strings.ToLower(strings.TrimSpace(filter))
	items := make([]tauui.DialogPickerItem, 0, len(instances)+1)
	for _, instance := range instances {
		if !pickerInstanceConnected(instance) {
			continue
		}
		name := sanitizeStatus(instance.Name)
		directory := sanitizeStatus(instance.CWD)
		state := sanitizeStatus(instance.StatusText())
		if instance.Outdated {
			state += " ⟳"
		}
		if instance.ID == current {
			state += " · current"
		}
		id := instance.ID[:min(8, len(instance.ID))]
		prompt := sanitizeStatus(instance.FirstPrompt)
		sessionName := sanitizeStatus(instance.SessionName)
		if !strings.Contains(strings.ToLower(strings.Join([]string{name, sessionName, prompt, directory, instance.ID, instance.SessionID, state}, " ")), filter) {
			continue
		}
		title := shortenHome(directory)
		if name != "" {
			title = name + " | " + title
		}
		items = append(items, tauui.DialogPickerItem{
			Name:        title,
			Description: id + " | " + state,
			Detail:      instanceSessionText(instance, sessionName, prompt),
			Value:       instance.ID,
		})
	}
	if strings.Contains(strings.ToLower("start new instance "+cwd), filter) {
		items = append(items, tauui.DialogPickerItem{Name: "Start a new instance", Detail: sanitizeStatus(cwd), Value: NewInstanceChoice})
	}
	return items
}

// instanceSessionText prefers a chosen session name over the first prompt. A
// name derived from the prompt is only a slug, so the prompt reads better.
func instanceSessionText(instance coord.Instance, sessionName, prompt string) string {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(instance.FirstPrompt), "\n")
	switch {
	case sessionName != "" && session.SlugFromName(strings.Join(strings.Fields(firstLine), " ")) != sessionName:
		return sessionName
	case prompt != "":
		return prompt
	case sessionName != "":
		return sessionName
	}
	return "No messages yet"
}

// PromptInstanceName edits a name in the picker's input. An empty result clears
// the name; ok is false when the user cancels.
func PromptInstanceName(ctx context.Context, instance coord.Instance) (name string, ok bool, err error) {
	root, err := runTerminalSelector(ctx, terminalSelector{
		header: "Name this instance — Enter save · Esc cancel\n" + sanitizeStatus(instance.CWD),
		title:  "tau — name instance",
		filter: instance.Name,
		items:  instanceNameItems,
	})
	if err != nil {
		return "", false, err
	}
	if root.selected == "" {
		return "", false, nil
	}
	return strings.TrimSpace(root.picker.Filter()), true, nil
}

func instanceNameItems(input string) []tauui.DialogPickerItem {
	if strings.TrimSpace(input) == "" {
		return []tauui.DialogPickerItem{{Name: "Clear name", Value: "save"}}
	}
	return []tauui.DialogPickerItem{{Name: "Save name", Value: "save"}}
}

// ConfirmInstanceStop requires an explicit choice before stopping shared work.
func ConfirmInstanceStop(ctx context.Context, instance coord.Instance) (bool, error) {
	choice, err := selectTerminalItem(ctx, "Stop this worker for all clients?\n"+sanitizeStatus(instance.CWD), "tau — stop instance", func(string) []tauui.DialogPickerItem {
		return []tauui.DialogPickerItem{{Name: "Keep running", Value: "keep"}, {Name: "Stop instance", Value: "stop"}}
	}, nil)
	return choice == "stop", err
}

// ConfirmInstanceManagement asks once before restarting or stopping workers
// that are busy or awaiting input. Idle targets proceed without a dialog.
func ConfirmInstanceManagement(ctx context.Context, action string, targets []coord.Instance) (bool, error) {
	prompt, ok := instanceManagementPrompt(action, targets)
	if !ok {
		return true, nil
	}
	choice, err := selectTerminalItem(ctx, prompt, "tau — confirm instance action", func(string) []tauui.DialogPickerItem {
		return []tauui.DialogPickerItem{{Name: "Cancel", Value: "cancel"}, {Name: instanceManagementVerb(action), Value: "confirm"}}
	}, nil)
	return choice == "confirm", err
}

// instanceManagementPrompt reports the confirmation text, or false when every
// target is idle.
func instanceManagementPrompt(action string, targets []coord.Instance) (string, bool) {
	var active []string
	for _, target := range targets {
		if target.Active() {
			active = append(active, sanitizeStatus(target.CWD))
		}
	}
	if len(active) == 0 {
		return "", false
	}
	noun := "this worker"
	if len(targets) > 1 {
		noun = fmt.Sprintf("%d workers", len(targets))
	}
	return fmt.Sprintf("%s %s for all clients? Working or awaiting input:\n%s", instanceManagementVerb(action), noun, strings.Join(active, "\n")), true
}

func instanceManagementVerb(action string) string {
	if action == InstanceActionStop {
		return "Stop"
	}
	return "Restart"
}
