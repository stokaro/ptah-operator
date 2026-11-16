package runner

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const (
	// codePlanRefused and codeApplyRefused are the frame codes of a plan and of
	// an approved plan's apply that Ptah refused under a code of its own.
	codePlanRefused  = "plan_refused"
	codeApplyRefused = "apply_refused"

	// maxRefusalSentenceBytes is what of a refusal's sentence fits an Event
	// with the longer code in front: the controller keeps 512 bytes of the
	// whole sentence there, as for maxDesiredSourceReasonBytes, so a longer
	// sentence would lose the count of the entries it does not name.
	maxRefusalSentenceBytes = 512 - len(codeApplyRefused) - len(": ")

	// namedRefusalEntries is how many of the entries a refusal lists its
	// sentence names before it counts the rest, as a migration's status names
	// versions (versionList).
	namedRefusalEntries = 8
)

// refusalSentence is how a refusal Ptah named by a code reads in a frame: the
// lead and the code, as in `ptah refused to plan the change (dependent-object)`.
//
// A refusal that lists what it refused names the entries after the code: the
// first few that fit maxRefusalSentenceBytes, then how many more there are, as
// in `(view-security-option: app.orders_view sql_security, and 2 more)`.
// refusalEntries says how each list reads. A first entry too long to fit is cut
// before the words Ptah names in its own vocabulary, such as the option, and
// the cut ends in "...".
//
// A refusal that lists nothing, or lists something in a form this runner does
// not read, reads as the lead and the code alone, as every refusal did before
// Ptah listed what it refused.
func refusalSentence(lead string, refusal *dataplane.SchemaRefusal, redactor Redactor) string {
	plain := lead + " (" + refusal.Code + ")"
	entries, ok := refusalEntries(refusal, redactor)
	if !ok {
		return plain
	}
	// What the list may take: the sentence less the lead, the code and its
	// parentheses, and the separator in front of the list.
	room := maxRefusalSentenceBytes - len(plain) - len(": ")
	shown := 0
	for shown < min(len(entries), namedRefusalEntries) &&
		len(joinRefusalEntries(entries[:shown+1])+andMore(shown+1, len(entries))) <= room {
		shown++
	}
	list := joinRefusalEntries(entries[:shown]) + andMore(shown, len(entries))
	if shown == 0 {
		// The cut keeps a few hundred bytes of what the first entry names: a
		// code, the option (an identifier of at most 64 bytes) and a count
		// take less than 200 of the 497, so the cut is never empty.
		const cut = "..."
		first, more := entries[0], andMore(1, len(entries))
		list = cutLine(first.named, room-len(more)-len(first.own)-len(cut)) + cut + first.own + more
	}
	return lead + " (" + refusal.Code + ": " + list + ")"
}

// refusalEntry is one thing a refusal lists, as its sentence names it: named
// is what came from a schema or a database, already through freeText, with the
// words that join its parts, and own is what follows it in words Ptah takes
// from a vocabulary of its own, which a cut leaves whole.
type refusalEntry struct {
	named, own string
}

func joinRefusalEntries(entries []refusalEntry) string {
	spelled := make([]string, 0, len(entries))
	for _, each := range entries {
		spelled = append(spelled, each.named+each.own)
	}
	return strings.Join(spelled, ", ")
}

// refusalEntries reads what a refusal lists, in the order Ptah lists it:
//
//   - each view and option of view_options (view-security-option), as
//     `orders_view sql_security`;
//   - each view of view_definers (view-definer), with the account it runs as
//     and the account the plan's read ran as, as
//     `orders_view definer app@% account deploy@%`, leaving out an account the
//     read did not record.
//
// A definer-privilege refusal lists the statements whose definer the login may
// not name, and this does not read that list, so it reads by its code alone
// (dataplane.SchemaRefusal says when an Apply meets it). The Pod log carries
// Ptah's sentence for it.
//
// A view and an account come from a schema or a database, so each goes through
// freeText, which redacts it, puts a space where a control or format character
// was and keeps it on one line. An option has to be an identifier, which the
// dataplane reader holds it to.
//
// It reports false when the refusal lists nothing, when a list it carries is in
// a form the readers refuse, and when nothing readable is left of a value that
// was there: an account left out would read as one the read did not record.
func refusalEntries(refusal *dataplane.SchemaRefusal, redactor Redactor) ([]refusalEntry, bool) {
	var entries []refusalEntry
	clean := func(text string) (string, bool) {
		cleaned := freeText(text, redactor, maxRefusalSentenceBytes)
		return cleaned, cleaned != ""
	}
	// labeled appends each label and value whose value is set; an empty value
	// is one Ptah did not record.
	labeled := func(text string, parts ...string) (string, bool) {
		for index := 0; index+1 < len(parts); index += 2 {
			if parts[index+1] == "" {
				continue
			}
			value, ok := clean(parts[index+1])
			if !ok {
				return "", false
			}
			text += " " + parts[index] + " " + value
		}
		return text, true
	}
	if len(refusal.ViewOptions) != 0 {
		named, ok := refusal.NamedViewOptions()
		if !ok {
			return nil, false
		}
		for _, each := range named {
			view, ok := clean(each.View)
			if !ok {
				return nil, false
			}
			entries = append(entries, refusalEntry{named: view, own: " " + each.Option})
		}
	}
	if len(refusal.ViewDefiners) != 0 {
		named, ok := refusal.NamedViewDefiners()
		if !ok {
			return nil, false
		}
		for _, each := range named {
			view, ok := clean(each.View)
			if !ok {
				return nil, false
			}
			text, ok := labeled(view, "definer", each.Definer, "account", each.Account)
			if !ok {
				return nil, false
			}
			entries = append(entries, refusalEntry{named: text})
		}
	}
	return entries, len(entries) > 0
}

// andMore counts what a list of total entries leaves out after naming shown.
func andMore(shown, total int) string {
	if more := total - shown; more > 0 {
		return fmt.Sprintf(", and %d more", more)
	}
	return ""
}
