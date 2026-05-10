package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wesm/msgvault/internal/query"
)

func TestSelectionToggle(t *testing.T) {
	model := NewBuilder().WithRows(
		makeRow("alice@example.com", 10),
		makeRow("bob@example.com", 5),
		makeRow("carol@example.com", 3),
	).Build()

	// Toggle selection with space
	model.cursor = 0
	model, _ = sendKey(t, model, key(' '))

	assertSelected(t, model, "alice@example.com")

	// Toggle off
	model, _ = sendKey(t, model, key(' '))

	assertNotSelected(t, model, "alice@example.com")
}

func TestSelectAllVisible(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("row1", 10), makeRow("row2", 9), makeRow("row3", 8),
			makeRow("row4", 7), makeRow("row5", 6), makeRow("row6", 5),
		).
		WithPageSize(3).
		Build()

	model = applyAggregateKey(t, model, key('S'))

	assertSelectionCount(t, model, 3)
	assertSelected(t, model, "row1")
	assertSelected(t, model, "row2")
	assertSelected(t, model, "row3")
	assertNotSelected(t, model, "row4")
	assertNotSelected(t, model, "row5")
	assertNotSelected(t, model, "row6")
}

func TestSelectAllVisibleWithScroll(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("row1", 10), makeRow("row2", 9), makeRow("row3", 8),
			makeRow("row4", 7), makeRow("row5", 6), makeRow("row6", 5),
		).
		WithPageSize(3).
		Build()
	model.scrollOffset = 2 // Scrolled down, showing row3-row5

	model = applyAggregateKey(t, model, key('S'))

	assertSelectionCount(t, model, 3)
	assertNotSelected(t, model, "row1")
	assertNotSelected(t, model, "row2")
	assertSelected(t, model, "row3")
	assertSelected(t, model, "row4")
	assertSelected(t, model, "row5")
	assertNotSelected(t, model, "row6")
}

func TestSelectionClearedOnViewSwitch(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model, 0)
	assertSelectionCount(t, model, 1)

	// Switch view with Tab
	model = applyAggregateKey(t, model, keyTab())

	assertSelectionCount(t, model, 0)
	assertSelectionViewTypeMatches(t, model)
}

func TestSelectionClearedOnShiftTab(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model, 0)

	// Switch view with Shift+Tab
	model = applyAggregateKey(t, model, keyShiftTab())

	assertSelectionCount(t, model, 0)
}

func TestClearSelection(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model, 0)
	assertSelectionCount(t, model, 1)

	// Clear with 'x'
	model = applyAggregateKey(t, model, key('x'))

	assertSelectionCount(t, model, 0)
}

// TestDKeyShowsReadOnlyNotice verifies the read-only-edition behavior:
// pressing `D` (or `d`) at the aggregate level does not stage anything
// and does not modify selection — it just surfaces a flash message
// explaining that deletion is disabled.
func TestDKeyShowsReadOnlyNotice(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 2)).
		WithGmailIDs("msg1", "msg2").
		Build()

	model = selectRow(t, model, 0)
	prevSelectionCount := model.selectionCount()

	model, _ = sendKey(t, model, key('D'))

	assertModal(t, model, modalNone)
	if model.flashMessage == "" {
		t.Error("expected flashMessage to be set after pressing D")
	}
	if !strings.Contains(strings.ToLower(model.flashMessage), "read-only") {
		t.Errorf("expected flash to mention 'read-only', got %q", model.flashMessage)
	}
	if got := model.selectionCount(); got != prevSelectionCount {
		t.Errorf("D should not modify selection; count was %d, now %d", prevSelectionCount, got)
	}
}

func TestAKeyShowsAllMessages(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 2)).
		Build()

	var cmd tea.Cmd
	model, cmd = sendKey(t, model, key('a'))

	assertLevel(t, model, levelMessageList)
	assertFilterKey(t, model, "")
	assertCmd(t, cmd, true)
	assertBreadcrumbCount(t, model, 1)
}

func TestSelectionCount(t *testing.T) {
	model := Model{
		selection: selectionState{
			aggregateKeys: map[string]bool{"a": true, "b": true},
			messageIDs:    map[int64]bool{1: true, 2: true, 3: true},
		},
	}

	if model.selectionCount() != 5 {
		t.Errorf("expected SelectionCount() = 5, got %d", model.selectionCount())
	}
}

func TestHasSelection(t *testing.T) {
	model := Model{
		selection: selectionState{
			aggregateKeys: make(map[string]bool),
			messageIDs:    make(map[int64]bool),
		},
	}

	if model.hasSelection() {
		t.Error("expected HasSelection() = false for empty selection")
	}

	model.selection.aggregateKeys["test"] = true
	if !model.hasSelection() {
		t.Error("expected HasSelection() = true with aggregate selection")
	}

	model.selection.aggregateKeys = make(map[string]bool)
	model.selection.messageIDs[1] = true
	if !model.hasSelection() {
		t.Error("expected HasSelection() = true with message selection")
	}
}

// TestDKeyDoesNotAutoSelectInReadOnly verifies that pressing `d` at the
// aggregate level no longer auto-selects the current row (the old
// stage-for-deletion behavior). It just shows the read-only flash.
func TestDKeyDoesNotAutoSelectInReadOnly(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("alice@example.com", 10),
			makeRow("bob@example.com", 5),
		).
		WithViewType(query.ViewSenders).
		Build()
	model.cursor = 1

	assertHasSelection(t, model, false)

	m := applyAggregateKey(t, model, key('d'))

	assertHasSelection(t, m, false)
	assertModal(t, m, modalNone)
	if m.flashMessage == "" {
		t.Error("expected read-only flash after pressing d")
	}
}

// TestMessageListDKeyDoesNotAutoSelectInReadOnly verifies the same for
// the message-list level.
func TestMessageListDKeyDoesNotAutoSelectInReadOnly(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, SourceMessageID: "msg1", Subject: "Hello"},
			query.MessageSummary{ID: 2, SourceMessageID: "msg2", Subject: "World"},
		).
		WithLevel(levelMessageList).
		Build()

	assertHasSelection(t, model, false)

	m := applyMessageListKey(t, model, key('d'))

	assertHasSelection(t, m, false)
	assertModal(t, m, modalNone)
	if m.flashMessage == "" {
		t.Error("expected read-only flash after pressing d")
	}
}

func TestToggleAggregateSelection(t *testing.T) {
	m := NewBuilder().WithRows(
		makeRow("alice@example.com", 0),
		makeRow("bob@example.com", 0),
	).Build()
	m.cursor = 0

	m.toggleAggregateSelection()
	if !m.selection.aggregateKeys["alice@example.com"] {
		t.Error("expected alice to be selected")
	}

	m.toggleAggregateSelection()
	if m.selection.aggregateKeys["alice@example.com"] {
		t.Error("expected alice to be deselected")
	}
}

func TestSelectVisibleAggregates(t *testing.T) {
	rows := make([]query.AggregateRow, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, query.AggregateRow{Key: fmt.Sprintf("user%d", i)})
	}
	m := NewBuilder().WithRows(rows...).Build()
	m.pageSize = 3
	m.scrollOffset = 2

	m.selectVisibleAggregates()

	for i := 2; i < 5; i++ {
		key := fmt.Sprintf("user%d", i)
		if !m.selection.aggregateKeys[key] {
			t.Errorf("expected %s to be selected", key)
		}
	}
	if m.selection.aggregateKeys["user0"] {
		t.Error("user0 should not be selected")
	}
}

func TestSelectVisibleAggregates_OffsetBeyondRows(t *testing.T) {
	m := NewBuilder().WithRows(makeRow("a", 0)).Build()
	m.scrollOffset = 100
	m.pageSize = 5

	m.selectVisibleAggregates()

	if len(m.selection.aggregateKeys) != 0 {
		t.Error("expected no selections when scrollOffset > len(rows)")
	}
}

func TestClearAllSelections(t *testing.T) {
	m := NewBuilder().WithRows(makeRow("a", 0)).Build()
	m.selection.aggregateKeys["a"] = true
	m.selection.messageIDs[1] = true

	m.clearAllSelections()

	if len(m.selection.aggregateKeys) != 0 || len(m.selection.messageIDs) != 0 {
		t.Error("expected all selections to be cleared")
	}
}
