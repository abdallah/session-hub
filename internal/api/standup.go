package api

import (
	"strings"
	"time"
)

// Standup is the answer to GET /v1/tasks/standup: a daily standup as plain
// text. Since is the earlier day its Yesterday part covers, or "" when no
// day in the week before had any work.
type Standup struct {
	Date  string `json:"date"`
	Since string `json:"since,omitempty"`
	Text  string `json:"text"`
}

// StandupText writes a standup from two day views: Yesterday is prev's done
// and worked-on tasks, Today is today's open and done tasks and its todo
// list, and Blockers is what today's tasks wait on. Ideas stay out.
func StandupText(prev *TaskDay, today TaskDay) string {
	var b strings.Builder
	head := "Yesterday"
	if prev != nil {
		if d, err := time.Parse("2006-01-02", prev.Date); err == nil {
			head += " (" + d.Format("Mon 2 Jan") + ")"
		}
	}
	var y []string
	if prev != nil {
		for _, t := range prev.Done {
			y = append(y, standupItem(t)+" (done)")
		}
		for _, t := range prev.InProgress {
			if t.ActiveSeconds > 0 {
				y = append(y, standupItem(t)+" (in progress)")
			}
		}
	}
	standupSection(&b, head, y)

	var td, blockers []string
	for _, t := range today.InProgress {
		td = append(td, standupItem(t))
		if len(t.WaitingOn) > 0 {
			blockers = append(blockers, standupItem(t)+": waiting on "+strings.Join(t.WaitingOn, "; "))
		}
	}
	for _, t := range today.Todo {
		td = append(td, standupItem(t))
	}
	for _, t := range today.Done {
		td = append(td, standupItem(t)+" (done)")
	}
	b.WriteString("\n")
	standupSection(&b, "Today", td)
	b.WriteString("\n")
	standupSection(&b, "Blockers", blockers)
	return b.String()
}

func standupItem(t Task) string {
	if t.Ref != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(t.Ref)) {
		return t.Ref + ": " + t.Title
	}
	return t.Title
}

func standupSection(b *strings.Builder, head string, items []string) {
	b.WriteString(head + ":\n")
	if len(items) == 0 {
		b.WriteString("- none\n")
	}
	for _, it := range items {
		b.WriteString("- " + it + "\n")
	}
}
