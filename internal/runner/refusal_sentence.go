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
	// sentence would lose the count of the views it does not name.
	maxRefusalSentenceBytes = 512 - len(codeApplyRefused) - len(": ")

	// namedRefusalEntries is how many of the entries a refusal lists its
	// sentence names before it counts the rest, as a migration's status names
	// versions (versionList).
	namedRefusalEntries = 8
)

// refusalSentence is how a refusal Ptah named by a code reads in a frame: the
// lead and the code, as in `ptah refused to plan the change (dependent-object)`.
//
// A refusal that lists views and options, as view-security-option does, names
// them after the code: the first few that fit maxRefusalSentenceBytes, each as
// the view and the option, then how many more there are, as in
// `(view-security-option: app.orders_view sql_security, and 2 more)`. A view
// comes from a schema or a database, so it goes through freeText, which
// redacts it, puts a space where a control or format character was and keeps
// it on one line. A first view too long to fit is cut, and ends in "...". An
// option is an identifier, which NamedViewOptions holds it to.
//
// A refusal that lists none, or lists them in a form NamedViewOptions does not
// read, reads as the lead and the code alone, as every refusal did before Ptah
// listed the views.
func refusalSentence(lead string, refusal *dataplane.SchemaRefusal, redactor Redactor) string {
	plain := lead + " (" + refusal.Code + ")"
	named, ok := refusal.NamedViewOptions()
	if !ok {
		return plain
	}
	entries := make([]string, 0, len(named))
	for _, each := range named {
		view := freeText(each.View, redactor, maxRefusalSentenceBytes)
		if view == "" {
			return plain
		}
		entries = append(entries, view+" "+each.Option)
	}
	// What the list may take: the sentence less the lead, the code and its
	// parentheses, and the separator in front of the list.
	room := maxRefusalSentenceBytes - len(plain) - len(": ")
	shown := 0
	for shown < min(len(entries), namedRefusalEntries) &&
		len(strings.Join(entries[:shown+1], ", ")+andMore(shown+1, len(entries))) <= room {
		shown++
	}
	list := strings.Join(entries[:shown], ", ") + andMore(shown, len(entries))
	if shown == 0 {
		// The cut keeps a few hundred bytes of a view the loop above found
		// readable: a code, an option and a count take less than 200 of the
		// 497, so the cut view is never empty.
		const cut = "..."
		option, more := " "+named[0].Option, andMore(1, len(entries))
		list = freeText(named[0].View, redactor, room-len(more)-len(option)-len(cut)) + cut + option + more
	}
	return lead + " (" + refusal.Code + ": " + list + ")"
}

// andMore counts what a list of total entries leaves out after naming shown.
func andMore(shown, total int) string {
	if more := total - shown; more > 0 {
		return fmt.Sprintf(", and %d more", more)
	}
	return ""
}
