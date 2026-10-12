package core

import "webweave/workspace"

func (a *App) runRoot(scope, id string) string { return workspace.ScopeRoot(a.root, scope, id) }

func (a *App) prepareDirectories(id string) error { return workspace.PrepareDirectories(a.root, id) }
