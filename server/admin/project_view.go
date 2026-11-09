package admin

import (
	"openfabrica/db/models"
	"openfabrica/internal/projectphase"
)

// projectView is a project as the admin UI receives it: the stored row plus the
// derived phase, so no client recomputes it from mode, run, and engine state.
type projectView struct {
	*models.Project
	Phase projectphase.Phase `json:"phase"`
}

// viewProject wraps one project. Its database is opened first so a single
// project reports run detail (waiting on questions) even when idle.
func (d *Deps) viewProject(p *models.Project) projectView {
	if d.ProjectDBManager != nil && p.Mode != models.ModeLive {
		_, _ = d.ProjectDBManager.Open(p.ID)
	}
	return d.wrapProject(p)
}

// viewProjects wraps a list. Only databases that are already open are read, so
// listing projects never opens every project's database.
func (d *Deps) viewProjects(ps []models.Project) []projectView {
	out := make([]projectView, len(ps))
	for i := range ps {
		out[i] = d.wrapProject(&ps[i])
	}
	return out
}

func (d *Deps) wrapProject(p *models.Project) projectView {
	running := d.Engine != nil && d.Engine.IsRunning(p.ID)
	return projectView{
		Project: p,
		Phase:   projectphase.Load(d.ProjectDBManager, running, p),
	}
}
