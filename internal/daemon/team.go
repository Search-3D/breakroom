package daemon

// This file serves teams: builders joining a series by name, team pairing
// for an architect's next series, and peer visibility scoped to teams.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/render"
	"github.com/search-3d/breakroom/internal/store"
)

// handleBlueprintJoin makes the caller the builder of a series: the named
// one, or the only open one. With several open it lists them; with none it
// records the caller as waiting, to be mailed when a series opens.
func (s *server) handleBlueprintJoin(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintJoinRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root and caller are required")
		return
	}
	ctx := r.Context()
	if _, err := s.store.GetAgent(ctx, req.WorkspaceRoot, req.Caller); err != nil {
		agentErr(w, err, req.Caller)
		return
	}
	name := req.Name
	if name == "" {
		open, err := s.store.ListOpenSeries(ctx, req.WorkspaceRoot)
		if err != nil {
			storeErr(w, err)
			return
		}
		switch len(open) {
		case 0:
			if err := s.store.AddSeriesWaiter(ctx, req.WorkspaceRoot, req.Caller); err != nil {
				storeErr(w, err)
				return
			}
			writeJSON(w, map[string]any{"joined": false, "waiting": true})
			return
		case 1:
			name = open[0].Name
		default:
			names := []string{}
			for _, b := range open {
				names = append(names, b.Name)
			}
			writeJSON(w, map[string]any{"joined": false, "open": names})
			return
		}
	}
	if err := s.store.JoinBlueprint(ctx, req.WorkspaceRoot, name, req.Caller); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if err := s.store.RemoveSeriesWaiter(ctx, req.WorkspaceRoot, req.Caller); err != nil {
		s.log.Error("series waiter cleanup failed", "agent", req.Caller, "error", err)
	}
	bp, err := s.store.GetBlueprint(ctx, req.WorkspaceRoot, name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.blueprintMail(ctx, req.WorkspaceRoot, req.Caller, bp.Architect, "blueprint_series",
		fmt.Sprintf("### [BLUEPRINT] %s - builder joined\n\n%s joined as the builder.\n", name, req.Caller))
	s.touchCaller(req.WorkspaceRoot, req.Caller)
	resp := map[string]any{"joined": true, "name": name, "architect": bp.Architect}
	if bp.WorktreePath != "" {
		s.moveBuilderIn(ctx, bp)
		resp["worktree"] = bp.WorktreePath
	}
	if n := bp.CurrentPhase; n > 0 && bp.Phases[n-1].Status == store.PhaseConstructing {
		// Specced before anyone joined: that mail had no recipient.
		ph := bp.Phases[n-1]
		s.blueprintMail(ctx, req.WorkspaceRoot, bp.Architect, req.Caller, "blueprint_spec",
			render.RenderSpec(bp.Name, n, ph.Title, ph.Objective, ph.Requirements, ph.Acceptance, ph.Constraints)+specWhere(bp))
	}
	writeJSON(w, resp)
}

// teamBuilder returns the builder an architect's next series should get: the
// builder of its most recent blueprint, if still online and not building
// another open blueprint.
func (s *server) teamBuilder(ctx context.Context, ws, architect string) (string, error) {
	b, err := s.store.LastBuilder(ctx, ws, architect)
	if err != nil || b == "" {
		return "", err
	}
	a, err := s.store.GetAgent(ctx, ws, b)
	if err != nil {
		if errors.Is(err, store.ErrNoAgent) {
			return "", nil
		}
		return "", err
	}
	if a.Status == "offline" {
		return "", nil
	}
	list, err := s.store.ListBlueprints(ctx, ws)
	if err != nil {
		return "", err
	}
	for _, bp := range list {
		if bp.Builder == b && bp.Status != store.BlueprintHandover {
			return "", nil
		}
	}
	return b, nil
}

// loneWaiter returns the single online builder waiting for a series, if
// exactly one is waiting; with two or more, notifyWaiters lets them choose.
func (s *server) loneWaiter(ctx context.Context, ws string) (string, error) {
	waiters, err := s.store.ListSeriesWaiters(ctx, ws)
	if err != nil || len(waiters) == 0 {
		return "", err
	}
	online := []string{}
	for _, id := range waiters {
		a, err := s.store.GetAgent(ctx, ws, id)
		if err != nil || a.Status == "offline" {
			_ = s.store.RemoveSeriesWaiter(ctx, ws, id)
			continue
		}
		online = append(online, id)
	}
	if len(online) == 1 {
		return online[0], nil
	}
	return "", nil
}

// notifyWaiters mails builders waiting for a series that one opened.
func (s *server) notifyWaiters(ctx context.Context, ws, name, architect string) {
	waiters, err := s.store.ListSeriesWaiters(ctx, ws)
	if err != nil {
		s.log.Error("series waiters failed", "error", err)
		return
	}
	for _, a := range waiters {
		s.blueprintMail(ctx, ws, architect, a, "blueprint_series",
			fmt.Sprintf("### [BLUEPRINT] Series %s opened\n\nIt needs a builder. Call blueprint_join with name %q.\n", name, name))
		if err := s.store.RemoveSeriesWaiter(ctx, ws, a); err != nil {
			s.log.Error("series waiter cleanup failed", "agent", a, "error", err)
		}
	}
}

// visiblePeers filters who an agent sees in peer lists and broadcasts: a builder
// sees its architect; an architect sees its builder, other architects, and
// unassigned agents. Direct messages can still go to anyone.
func (s *server) visiblePeers(ctx context.Context, ws, me string, all []*store.Agent) []*store.Agent {
	list, err := s.store.ListBlueprints(ctx, ws)
	if err != nil {
		s.log.Error("visible peers: list blueprints", "error", err)
		list = nil // fail open: everyone visible
	}
	architects := map[string]bool{}
	builders := map[string]bool{}
	team := map[string]bool{} // agents sharing an open blueprint with me
	for _, bp := range list {
		if bp.Status == store.BlueprintHandover {
			continue
		}
		architects[bp.Architect] = true
		if bp.Builder != "" {
			builders[bp.Builder] = true
		}
		if bp.Architect == me && bp.Builder != "" {
			team[bp.Builder] = true
		}
		if bp.Builder == me {
			team[bp.Architect] = true
		}
	}
	out := []*store.Agent{}
	for _, a := range all {
		x := a.AgentID
		if x == me {
			continue
		}
		roleless := !architects[x] && !builders[x]
		visible := false
		switch {
		case !architects[me] && !builders[me]:
			visible = true
		case architects[me] && (team[x] || architects[x] || roleless):
			visible = true
		case builders[me] && team[x]:
			visible = true
		}
		if visible {
			out = append(out, a)
		}
	}
	return out
}
