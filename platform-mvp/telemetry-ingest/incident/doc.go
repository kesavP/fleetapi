// Package incident is the lifecycle of an incident: what it is, which states it can be in and how it moves between them, the timeline of
// what happened to it, and the references to the evidence behind it. Nothing here touches a network or a database, so all of it can be
// tested directly; the store and the HTTP handlers are thin shells around it.
//
// An incident is never updated in place. Everything that happens to it is an EVENT, appended to a log that is never edited, and the incident
// as it stands is the fold of its events (Fold). That gives an audit trail for free (who did what, when), makes every change a single
// append (ClickHouse has no row updates and no transactions), and means the rules below are the only way a state can come about: a log
// that breaks them does not fold.
//
// States and moves (anything else is refused, and the refusal says what is allowed):
//
//	open          -> acknowledged, investigating, mitigated, resolved
//	acknowledged  -> investigating, mitigated, resolved
//	investigating -> mitigated, resolved
//	mitigated     -> investigating (it came back), resolved
//	resolved      -> closed, open (reopened)
//	closed        -> open (reopened)
//
// Resolving needs a resolution (fixed, workaround, false_positive, duplicate or not_reproducible); reopening needs a note saying why.
package incident
