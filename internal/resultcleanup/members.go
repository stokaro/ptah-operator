package resultcleanup

import (
	"context"
	"sort"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func selector(key string, op selection.Operator, values ...string) labels.Selector {
	r, err := labels.NewRequirement(key, op, values)
	if err != nil {
		panic(err) // All callers supply fixed, valid label names and operators.
	}
	return labels.NewSelector().Add(*r)
}

func metadataList() *metav1.PartialObjectMetadataList {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(api.GroupVersion.WithKind("PtahResultRecordList"))
	return list
}

// members lists an attempt's chunks and completion through the attempt index.
// Every record carries it: admission refuses a publication without one. One
// bounded page holds them all, so an incomplete listing, or a member of the
// index another attempt owns, refuses cleanup rather than assuming an empty
// collection.
func (p Policy) members(ctx context.Context, b resultstore.Binding) ([]metav1.PartialObjectMetadata, error) {
	name, err := resultstore.Name(b)
	if err != nil {
		return nil, err
	}
	index, _ := selector(resultstore.LabelAttempt, selection.Equals, resultstore.AttemptLabel(name)).Requirements()
	query := selector(resultstore.LabelRecord, selection.In, "chunk", "complete").Add(index...)
	list := metadataList()
	if err := p.Reader.List(ctx, list, &client.ListOptions{Namespace: b.Namespace, LabelSelector: query, Limit: 128}); err != nil {
		return nil, err
	}
	found := list.Items
	if list.Continue != "" || len(found) > 128 {
		return nil, ErrRetained
	}
	for _, member := range found {
		if len(member.OwnerReferences) != 1 || member.OwnerReferences[0].Name != name || member.OwnerReferences[0].Kind != "PtahResultRecord" {
			return nil, ErrRetained
		}
	}
	// Withdraw completion before removing any of its chunks.
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if (a.Labels[resultstore.LabelRecord] == "complete") != (b.Labels[resultstore.LabelRecord] == "complete") {
			return a.Labels[resultstore.LabelRecord] == "complete"
		}
		return a.Name < b.Name
	})
	return found, nil
}
