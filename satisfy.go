package vsolver

import "github.com/Sirupsen/logrus"

// satisfiable is the main checking method. It determines if introducing a new
// project atom would result in a state where all solver requirements are still
// satisfied.
func (s *solver) satisfiable(pa ProjectAtom) error {
	if emptyProjectAtom == pa {
		// TODO we should protect against this case elsewhere, but for now panic
		// to canary when it's a problem
		panic("canary - checking version of empty ProjectAtom")
	}

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"name":    pa.Ident,
			"version": pa.Version,
		}).Debug("Checking satisfiability of project atom against current constraints")
	}

	if err := s.checkAtomAllowable(pa); err != nil {
		return err
	}

	deps, err := s.getDependenciesOf(pa)
	if err != nil {
		// An err here would be from the package fetcher; pass it straight back
		return err
	}

	for _, dep := range deps {
		if err := s.checkIdentMatches(pa, dep); err != nil {
			return err
		}
		if err := s.checkDepsConstraintsAllowable(pa, dep); err != nil {
			return err
		}
		if err := s.checkDepsDisallowsSelected(pa, dep); err != nil {
			return err
		}

		// TODO add check that fails if adding this atom would create a loop
	}

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"name":    pa.Ident,
			"version": pa.Version,
		}).Debug("Project atom passed satisfiability test against current state")
	}

	return nil
}

// checkAtomAllowable ensures that an atom itself is acceptable with respect to
// the constraints established by the current solution.
func (s *solver) checkAtomAllowable(pa ProjectAtom) error {
	constraint := s.sel.getConstraint(pa.Ident)
	if constraint.Matches(pa.Version) {
		return nil
	}
	// TODO collect constraint failure reason

	if s.l.Level >= logrus.InfoLevel {
		s.l.WithFields(logrus.Fields{
			"name":          pa.Ident,
			"version":       pa.Version,
			"curconstraint": constraint.String(),
		}).Info("Current constraints do not allow version")
	}

	deps := s.sel.getDependenciesOn(pa.Ident)
	var failparent []Dependency
	for _, dep := range deps {
		if !dep.Dep.Constraint.Matches(pa.Version) {
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithFields(logrus.Fields{
					"name":       pa.Ident,
					"othername":  dep.Depender.Ident,
					"constraint": dep.Dep.Constraint.String(),
				}).Debug("Marking other, selected project with conflicting constraint as failed")
			}
			s.fail(dep.Depender.Ident)
			failparent = append(failparent, dep)
		}
	}

	err := &versionNotAllowedFailure{
		goal:       pa,
		failparent: failparent,
		c:          constraint,
	}

	s.logSolve(err)
	return err
}

// checkDepsConstraintsAllowable checks that the constraints of an atom on a
// given dep would not result in UNSAT.
func (s *solver) checkDepsConstraintsAllowable(pa ProjectAtom, dep ProjectDep) error {
	constraint := s.sel.getConstraint(dep.Ident)
	// Ensure the constraint expressed by the dep has at least some possible
	// intersection with the intersection of existing constraints.
	if constraint.MatchesAny(dep.Constraint) {
		return nil
	}

	if s.l.Level >= logrus.DebugLevel {
		s.l.WithFields(logrus.Fields{
			"name":          pa.Ident,
			"version":       pa.Version,
			"depname":       dep.Ident,
			"curconstraint": constraint.String(),
			"newconstraint": dep.Constraint.String(),
		}).Debug("Project atom cannot be added; its constraints are disjoint with existing constraints")
	}

	siblings := s.sel.getDependenciesOn(dep.Ident)
	// No admissible versions - visit all siblings and identify the disagreement(s)
	var failsib []Dependency
	var nofailsib []Dependency
	for _, sibling := range siblings {
		if !sibling.Dep.Constraint.MatchesAny(dep.Constraint) {
			if s.l.Level >= logrus.DebugLevel {
				s.l.WithFields(logrus.Fields{
					"name":          pa.Ident,
					"version":       pa.Version,
					"depname":       sibling.Depender.Ident,
					"sibconstraint": sibling.Dep.Constraint.String(),
					"newconstraint": dep.Constraint.String(),
				}).Debug("Marking other, selected project as failed because its constraint is disjoint with our testee")
			}
			s.fail(sibling.Depender.Ident)
			failsib = append(failsib, sibling)
		} else {
			nofailsib = append(nofailsib, sibling)
		}
	}

	err := &disjointConstraintFailure{
		goal:      Dependency{Depender: pa, Dep: dep},
		failsib:   failsib,
		nofailsib: nofailsib,
		c:         constraint,
	}
	s.logSolve(err)
	return err
}

// checkDepsDisallowsSelected ensures that an atom's constraints on a particular
// dep are not incompatible with the version of that dep that's already been
// selected.
func (s *solver) checkDepsDisallowsSelected(pa ProjectAtom, dep ProjectDep) error {
	selected, exists := s.sel.selected(dep.Ident)
	if exists && !dep.Constraint.Matches(selected.Version) {
		if s.l.Level >= logrus.DebugLevel {
			s.l.WithFields(logrus.Fields{
				"name":          pa.Ident,
				"version":       pa.Version,
				"depname":       dep.Ident,
				"curversion":    selected.Version,
				"newconstraint": dep.Constraint.String(),
			}).Debug("Project atom cannot be added; a constraint it introduces does not allow a currently selected version")
		}
		s.fail(dep.Ident)

		err := &constraintNotAllowedFailure{
			goal: Dependency{Depender: pa, Dep: dep},
			v:    selected.Version,
		}
		s.logSolve(err)
		return err
	}
	return nil
}

// checkIdentMatches ensures that the LocalName of a dep introduced by an atom,
// has the same NetworkName as what's already been selected (assuming anything's
// been selected).
//
// In other words, this ensures that the solver never simultaneously selects two
// identifiers with the same local name, but that disagree about where their
// network source is.
func (s *solver) checkIdentMatches(pa ProjectAtom, dep ProjectDep) error {
	if cur, exists := s.names[dep.Ident.LocalName]; exists {
		if cur != dep.Ident.netName() {
			deps := s.sel.getDependenciesOn(pa.Ident)
			// Fail all the other deps, as there's no way atom can ever be
			// compatible with them
			for _, d := range deps {
				s.fail(d.Depender.Ident)
			}

			err := &sourceMismatchFailure{
				shared:   dep.Ident.LocalName,
				sel:      deps,
				current:  cur,
				mismatch: dep.Ident.netName(),
				prob:     pa,
			}
			s.logSolve(err)
			return err
		}
	}

	return nil
}
