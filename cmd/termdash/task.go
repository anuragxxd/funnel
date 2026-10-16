package termdash

import (
	"github.com/calypr/funnel/cmd/termdash/compact"
	"github.com/calypr/funnel/tes"
)

type TaskWidget struct {
	Task    *tes.Task
	Widgets *compact.Compact
	display bool
}

func NewTaskWidget(t *tes.Task) *TaskWidget {
	widgets := compact.NewCompact(t)
	return &TaskWidget{
		Task:    t,
		Widgets: widgets,
		display: true,
	}
}
