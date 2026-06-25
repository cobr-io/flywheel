// Package reconcile computes and categorises the change set `flywheel up`
// would apply (design § up step 4), and enforces the deletion tiering
// (design § up step 12 + § Local profiles): CRDs and PVCs are NEVER
// auto-deleted, even with --yes — they're surfaced as orphaned for
// `flywheel clean`.
package reconcile

import (
	"fmt"
	"sort"
	"strings"
)

// Op is the kind of change for one resource.
type Op int

const (
	// Additive: the resource is new (create).
	Additive Op = iota
	// Mutating: the resource exists and changes (update).
	Mutating
	// Destructive: the resource is removed (delete).
	Destructive
)

func (o Op) Symbol() string {
	switch o {
	case Additive:
		return "+"
	case Mutating:
		return "~"
	case Destructive:
		return "-"
	}
	return "?"
}

// Tier controls deletion handling for a Destructive change.
type Tier int

const (
	// Normal: deletable with --yes.
	Normal Tier = iota
	// OrphanPVC: a PersistentVolumeClaim — never auto-deleted; needs
	// `flywheel clean`.
	OrphanPVC
	// OrphanCRD: a CustomResourceDefinition — never auto-deleted (would
	// cascade-delete every CR, incl. user-created ones). CRDs are never
	// auto-removed; an operator must delete them manually if unused.
	OrphanCRD
)

// Change is one resource's planned change.
type Change struct {
	Group     string // API group ("" for core)
	Kind      string
	Namespace string // "" for cluster-scoped
	Name      string
	Op        Op
	Tier      Tier
}

func (c Change) GVKLabel() string {
	if c.Group == "" {
		return c.Kind
	}
	return c.Group + "/" + c.Kind
}

func (c Change) String() string {
	ns := c.Namespace
	if ns == "" {
		ns = "-"
	}
	return fmt.Sprintf("%s %s %s/%s", c.Op.Symbol(), c.GVKLabel(), ns, c.Name)
}

// ClassifyRemoval returns the Tier for a resource being removed. CRDs and
// PVCs are tiered out of auto-deletion per the design.
func ClassifyRemoval(group, kind string) Tier {
	switch {
	case group == "apiextensions.k8s.io" && kind == "CustomResourceDefinition":
		return OrphanCRD
	case group == "" && kind == "PersistentVolumeClaim":
		return OrphanPVC
	default:
		return Normal
	}
}

// Plan is the full categorised change set.
type Plan struct {
	Changes []Change
}

func (p *Plan) Add(c Change) {
	if c.Op == Destructive {
		c.Tier = ClassifyRemoval(c.Group, c.Kind)
	}
	p.Changes = append(p.Changes, c)
}

func (p *Plan) filter(pred func(Change) bool) []Change {
	var out []Change
	for _, c := range p.Changes {
		if pred(c) {
			out = append(out, c)
		}
	}
	return out
}

// Additive returns the +additive changes.
func (p *Plan) Additive() []Change {
	return p.filter(func(c Change) bool { return c.Op == Additive })
}

// DeletableDestructive returns destructive changes that CAN be
// auto-deleted (Tier == Normal). These are what step 12 deletes once
// approved.
func (p *Plan) DeletableDestructive() []Change {
	return p.filter(func(c Change) bool { return c.Op == Destructive && c.Tier == Normal })
}

// Orphaned returns destructive changes that are NEVER auto-deleted
// (CRDs + PVCs) — reported to the user for `flywheel clean`.
func (p *Plan) Orphaned() []Change {
	return p.filter(func(c Change) bool { return c.Op == Destructive && c.Tier != Normal })
}

// HasDeletableDestructive reports whether any non-tiered destructive
// change exists (the thing that requires --yes).
func (p *Plan) HasDeletableDestructive() bool {
	return len(p.DeletableDestructive()) > 0
}

// Approval is the user's gate decision.
type Approval struct {
	Yes         bool // --yes: approve everything deletable
	YesAdditive bool // --yes-additive: approve only +additive
}

// NeedsConfirmation reports whether the plan has deletable-destructive
// changes that the given approval does NOT cover. --yes-additive never
// covers destructive ops, so a plan with deletable-destructive changes
// needs --yes (or interactive confirm).
func (p *Plan) NeedsConfirmation(a Approval) bool {
	if !p.HasDeletableDestructive() {
		return false
	}
	return !a.Yes
}

// Render returns a terraform-plan-style summary, sorted for stable
// output.
func (p *Plan) Render() string {
	changes := append([]Change(nil), p.Changes...)
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Op != changes[j].Op {
			return changes[i].Op < changes[j].Op
		}
		return changes[i].String() < changes[j].String()
	})
	var b strings.Builder
	add, mut, del, orphan := 0, 0, 0, 0
	for _, c := range changes {
		b.WriteString("  ")
		b.WriteString(c.String())
		switch {
		case c.Op == Additive:
			add++
		case c.Op == Mutating:
			mut++
		case c.Op == Destructive && c.Tier == OrphanCRD:
			b.WriteString("   (orphaned: CRDs are never auto-removed; delete manually if unused)")
			orphan++
		case c.Op == Destructive && c.Tier == OrphanPVC:
			b.WriteString("   (orphaned: needs `flywheel clean`)")
			orphan++
		case c.Op == Destructive:
			del++
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Plan: %d to add, %d to change, %d to destroy, %d orphaned.\n",
		add, mut, del, orphan)
	return b.String()
}
