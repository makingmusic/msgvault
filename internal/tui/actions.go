package tui

import (
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wesm/msgvault/internal/export"
	"github.com/wesm/msgvault/internal/query"
)

// ExportResultMsg is returned when attachment export completes.
type ExportResultMsg struct {
	Result string
	Err    error
}

// ActionController handles business logic for actions like attachment
// export, keeping domain operations out of the TUI Model. The
// read-only edition of msgvault contains no remote-mutation actions
// (deletion, trash, label-change, etc.); only local operations remain.
type ActionController struct {
	queries query.Engine
	dataDir string
}

// NewActionController creates a new action controller.
func NewActionController(queries query.Engine, dataDir string) *ActionController {
	return &ActionController{
		queries: queries,
		dataDir: dataDir,
	}
}

// ExportAttachments performs the export logic.
func (c *ActionController) ExportAttachments(detail *query.MessageDetail, selection map[int]bool) tea.Cmd {
	if detail == nil || len(detail.Attachments) == 0 {
		return nil
	}

	var selectedAttachments []query.AttachmentInfo
	for i, att := range detail.Attachments {
		if selection[i] {
			selectedAttachments = append(selectedAttachments, att)
		}
	}

	if len(selectedAttachments) == 0 {
		return nil
	}

	attachmentsDir := filepath.Join(c.dataDir, "attachments")
	subject := detail.Subject
	if subject == "" {
		subject = "attachments"
	}
	subject = export.SanitizeFilename(subject)
	if len(subject) > 50 {
		subject = subject[:50]
	}
	zipFilename := fmt.Sprintf("%s_%d.zip", subject, detail.ID)

	return func() tea.Msg {
		stats := export.Attachments(zipFilename, attachmentsDir, selectedAttachments)
		msg := ExportResultMsg{Result: export.FormatExportResult(stats)}
		// Only set Err for true failures: write errors or zero exported files.
		// Partial success (some files exported, some errors) should show the
		// detailed Result which includes both the success info and error list.
		if stats.WriteError || stats.Count == 0 {
			msg.Err = fmt.Errorf("export failed")
		}
		return msg
	}
}
