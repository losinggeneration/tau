package tui

import (
	"fmt"
	"slices"
	"strings"

	tauui "github.com/losinggeneration/tau/tui"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/sdk"
)

type scopedModelItem struct {
	Key, ID, Provider string
}

type scopedModelsOverlay struct {
	app     *App
	items   []scopedModelItem
	focused int
	dirty   bool
}

type scopedModelsAction int

const (
	scopedModelsActionAll scopedModelsAction = iota
	scopedModelsActionClear
	scopedModelsActionProvider
	scopedModelsActionMoveUp
	scopedModelsActionMoveDown
	scopedModelsActionSave
)

func newScopedModelsOverlay(app *App) *scopedModelsOverlay {
	o := &scopedModelsOverlay{app: app}
	if app == nil {
		return o
	}
	models := app.availableModels()
	o.items = make([]scopedModelItem, 0, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		key := model.Provider + "/" + model.ID
		o.items = append(o.items, scopedModelItem{Key: key, ID: model.ID, Provider: model.Provider})
	}
	o.syncItemOrder()
	return o
}

func (o *scopedModelsOverlay) Render(width int) []string {
	if o == nil {
		return nil
	}
	if width <= 0 {
		width = 80
	}
	rows := []string{"Scoped models", "Enter toggle · Ctrl+A all · Ctrl+X clear · Ctrl+P provider · Alt+↑↓ reorder · Ctrl+S save"}
	if len(o.items) == 0 {
		return append(rows, "No authenticated models available")
	}
	for i, item := range o.items {
		marker := " "
		if i == o.focused {
			marker = "→"
		}
		checked := "☐"
		if o.app != nil && o.app.scopedModelEnabled(item.Key) {
			checked = "☑"
		}
		row := fmt.Sprintf("%s %s %s", marker, checked, item.ID)
		if item.Provider != "" {
			row += "  " + item.Provider
		}
		rows = append(rows, tauui.TextTruncate(row, width, false))
	}
	countText := "all enabled"
	if o.app != nil && o.app.scopedModelKeys != nil {
		countText = fmt.Sprintf("%d/%d enabled", len(o.app.scopedModelKeys), len(o.items))
	}
	if o.dirty {
		countText += " (unsaved)"
	}
	return append(rows, tauui.TextTruncate(countText, width, false))
}

func (o *scopedModelsOverlay) HandleKey(ke tauui.KeyEvent) {
	if o == nil {
		return
	}
	switch ke.Key {
	case tauui.KeyUp:
		if o.focused > 0 {
			o.focused--
		}
	case tauui.KeyDown:
		if o.focused < len(o.items)-1 {
			o.focused++
		}
	case tauui.KeyEnter:
		o.toggleFocused()
	case tauui.KeyRune:
		if ke.Rune == ' ' {
			o.toggleFocused()
		}
	}
}

func (o *scopedModelsOverlay) HandleAction(action scopedModelsAction) {
	if o == nil || o.app == nil {
		return
	}
	switch action {
	case scopedModelsActionAll:
		o.app.enableScopedModels(o.filteredKeys())
		o.markDirty()
	case scopedModelsActionClear:
		o.app.clearScopedModels(o.filteredKeys())
		o.markDirty()
	case scopedModelsActionProvider:
		item, ok := o.focusedItem()
		if !ok {
			return
		}
		o.app.toggleScopedModelProvider(item.Provider, o.allKeysForProvider(item.Provider))
		o.markDirty()
	case scopedModelsActionMoveUp:
		if o.moveFocused(-1) {
			o.markDirty()
		}
	case scopedModelsActionMoveDown:
		if o.moveFocused(1) {
			o.markDirty()
		}
	case scopedModelsActionSave:
		if err := o.app.saveScopedModels(); err != nil {
			if o.app.conv != nil {
				o.app.conv.AddError("scoped models: save: " + err.Error())
				o.app.refreshChat()
			}
			return
		}
		o.dirty = false
		if o.app.footer != nil && o.app.footer.data != nil {
			o.app.footer.data.notice = "scoped models saved"
		}
	}
}

func (o *scopedModelsOverlay) markDirty() {
	if o != nil {
		o.dirty = true
	}
}

func (o *scopedModelsOverlay) focusedItem() (scopedModelItem, bool) {
	if o == nil || o.focused < 0 || o.focused >= len(o.items) {
		return scopedModelItem{}, false
	}
	return o.items[o.focused], true
}

func (o *scopedModelsOverlay) filteredKeys() []string {
	if o == nil {
		return nil
	}
	keys := make([]string, 0, len(o.items))
	for _, item := range o.items {
		keys = append(keys, item.Key)
	}
	return keys
}

func (o *scopedModelsOverlay) allKeysForProvider(provider string) []string {
	if o == nil {
		return nil
	}
	keys := make([]string, 0)
	for _, item := range o.items {
		if item.Provider == provider {
			keys = append(keys, item.Key)
		}
	}
	return keys
}

func (o *scopedModelsOverlay) moveFocused(delta int) bool {
	item, ok := o.focusedItem()
	if !ok || o.app == nil {
		return false
	}
	if o.app.moveScopedModel(item.Key, delta) {
		o.syncItemOrder()
		for i, existing := range o.items {
			if existing.Key == item.Key {
				o.focused = i
				break
			}
		}
		return true
	}
	return false
}

func (o *scopedModelsOverlay) syncItemOrder() {
	if o == nil || o.app == nil || o.app.scopedModelKeys == nil {
		return
	}
	order := make(map[string]int, len(o.app.scopedModelKeys))
	for i, key := range o.app.scopedModelKeys {
		order[key] = i
	}
	slices.SortStableFunc(o.items, func(a, b scopedModelItem) int {
		ai, aok := order[a.Key]
		bi, bok := order[b.Key]
		switch {
		case aok && bok:
			return ai - bi
		case aok:
			return -1
		case bok:
			return 1
		default:
			return 0
		}
	})
}

func (o *scopedModelsOverlay) toggleFocused() {
	if o == nil || o.app == nil || o.focused < 0 || o.focused >= len(o.items) {
		return
	}
	o.app.toggleScopedModel(o.items[o.focused].Key)
	o.markDirty()
}

func (a *App) scopedModelsOverlay() (*scopedModelsOverlay, bool) {
	if a == nil || a.dialogPicker == nil || !a.dialogPicker.Shown() {
		return nil, false
	}
	o, ok := a.dialogPicker.Component().(*scopedModelsOverlay)
	return o, ok
}

func (a *App) scopedModelsDialogVisible() bool {
	_, ok := a.scopedModelsOverlay()
	return ok
}

func (a *App) handleScopedModelsAction(action scopedModelsAction, ctx *tauui.Ctx) bool {
	o, ok := a.scopedModelsOverlay()
	if !ok {
		return false
	}
	o.HandleAction(action)
	if ctx != nil && a.dialogPicker != nil {
		ctx.Invalidate(a.dialogPicker.Rect())
	}
	return true
}

func (a *App) scopedModelEnabled(key string) bool {
	if a == nil || key == "" {
		return false
	}
	if a.scopedModelKeys == nil {
		return true
	}
	return slicesContains(a.scopedModelKeys, key)
}

func (a *App) toggleScopedModel(key string) {
	if a == nil || key == "" {
		return
	}
	if a.scopedModelKeys == nil {
		a.scopedModelKeys = []string{key}
		a.reportScopedModels()
		return
	}
	for i, existing := range a.scopedModelKeys {
		if existing == key {
			a.scopedModelKeys = append(a.scopedModelKeys[:i], a.scopedModelKeys[i+1:]...)
			a.reportScopedModels()
			return
		}
	}
	a.scopedModelKeys = append(a.scopedModelKeys, key)
	a.reportScopedModels()
}

func (a *App) enableScopedModels(keys []string) {
	if a == nil {
		return
	}
	if a.scopedModelKeys == nil {
		return
	}
	for _, key := range keys {
		if !slicesContains(a.scopedModelKeys, key) {
			a.scopedModelKeys = append(a.scopedModelKeys, key)
		}
	}
	if len(a.scopedModelKeys) == len(a.availableModels()) {
		a.scopedModelKeys = nil
	}
	a.reportScopedModels()
}

func (a *App) clearScopedModels(keys []string) {
	if a == nil {
		return
	}
	if a.scopedModelKeys == nil {
		all := a.allScopedModelKeys()
		remove := stringSet(keys)
		a.scopedModelKeys = make([]string, 0, len(all))
		for _, key := range all {
			if _, ok := remove[key]; !ok {
				a.scopedModelKeys = append(a.scopedModelKeys, key)
			}
		}
		a.reportScopedModels()
		return
	}
	remove := stringSet(keys)
	a.scopedModelKeys = slices.DeleteFunc(a.scopedModelKeys, func(key string) bool {
		_, ok := remove[key]
		return ok
	})
	a.reportScopedModels()
}

func (a *App) toggleScopedModelProvider(provider string, keys []string) {
	if a == nil || provider == "" || len(keys) == 0 {
		return
	}
	allEnabled := true
	for _, key := range keys {
		if !a.scopedModelEnabled(key) {
			allEnabled = false
			break
		}
	}
	if allEnabled {
		a.clearScopedModels(keys)
		return
	}
	a.enableScopedModels(keys)
}

func (a *App) moveScopedModel(key string, delta int) bool {
	if a == nil || a.scopedModelKeys == nil || key == "" || delta == 0 {
		return false
	}
	idx := slices.Index(a.scopedModelKeys, key)
	if idx < 0 {
		return false
	}
	next := idx + delta
	if next < 0 || next >= len(a.scopedModelKeys) {
		return false
	}
	a.scopedModelKeys[idx], a.scopedModelKeys[next] = a.scopedModelKeys[next], a.scopedModelKeys[idx]
	a.reportScopedModels()
	return true
}

func (a *App) saveScopedModels() error {
	if a == nil || a.settingsPath == "" {
		return nil
	}
	settings, err := ai.LoadSettings(a.settingsPath)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if a.scopedModelKeys == nil {
		settings.EnabledModels = nil
	} else {
		settings.EnabledModels = append([]string(nil), a.scopedModelKeys...)
	}
	if err := ai.SaveSettings(a.settingsPath, settings); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

func (a *App) allScopedModelKeys() []string {
	if a == nil {
		return nil
	}
	models := a.availableModels()
	keys := make([]string, 0, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		keys = append(keys, model.Provider+"/"+model.ID)
	}
	return keys
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func (a *App) reportScopedModels() {
	if a == nil || a.footer == nil || a.footer.data == nil {
		return
	}
	if a.scopedModelKeys == nil {
		a.footer.data.notice = "scoped models: all"
		return
	}
	if len(a.scopedModelKeys) == 0 {
		a.footer.data.notice = "scoped models: none"
		return
	}
	a.footer.data.notice = fmt.Sprintf("scoped models: %d", len(a.scopedModelKeys))
}

func (a *App) cycleScopedModel() bool {
	if a == nil || a.sess == nil {
		return false
	}
	if a.scopedModelKeys == nil {
		cycler, ok := a.sess.(sdk.ModelCycler)
		if !ok {
			return false
		}
		model, err := cycler.CycleModel()
		if err != nil {
			a.conv.AddError("model cycle: " + err.Error())
			a.refreshChat()
			return true
		}
		if model == nil {
			return false
		}
		key := model.Provider + "/" + model.ID
		a.currentModel = key
		if a.footer != nil && a.footer.data != nil {
			a.footer.data.model = key
		}
		a.refreshFooterStats(a.loopContext())
		return true
	}
	if len(a.scopedModelKeys) == 0 {
		a.conv.AddError("model cycle: no scoped models enabled")
		a.refreshChat()
		return true
	}
	current := a.currentModel
	if current == "" && a.sess != nil {
		current = a.sess.State().Model()
	}
	idx := -1
	for i, key := range a.scopedModelKeys {
		if key == current {
			idx = i
			break
		}
	}
	next := a.scopedModelKeys[(idx+1)%len(a.scopedModelKeys)]
	a.selectModel(next, a.scopedThinking[next])
	return true
}

func cmdScopedModelsOverlay(app *App, _ string) {
	if app == nil || app.dialogPicker == nil {
		return
	}
	app.closeDialogComponent()
	overlay := newScopedModelsOverlay(app)
	app.dialogPicker.SetComponent(overlay)
	app.dialogPicker.SetHeader("/scoped-models")
	app.dialogPicker.Reset()
	app.dialogPicker.SetShown(true)
	if ctx := app.loopContext(); ctx != nil && ctx.InvalidateLayout != nil {
		ctx.InvalidateLayout()
	}
}

func slicesContains(values []string, value string) bool {
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return true
		}
	}
	return false
}
