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

// Members uses the immutable attempt index for new records. The bounded legacy
// scan considers only unindexed children, a set new publications cannot grow.
// An incomplete scan refuses cleanup rather than assuming an empty collection.
func (p Policy) members(ctx context.Context, b resultstore.Binding) ([]metav1.PartialObjectMetadata, error) {
	name, err := resultstore.Name(b)
	if err != nil {
		return nil, err
	}
	roles := selector(resultstore.LabelRecord, selection.In, "chunk", "complete")
	var found []metav1.PartialObjectMetadata
	for _, legacy := range []bool{false, true} {
		index := selector(resultstore.LabelAttempt, selection.Equals, resultstore.AttemptLabel(name))
		pages := 1
		if legacy {
			index = selector(resultstore.LabelAttempt, selection.DoesNotExist)
			pages = 16
		}
		requirements, _ := index.Requirements()
		query := roles.Add(requirements...)
		continuation := ""
		for page := 0; page < pages; page++ {
			list := metadataList()
			if err := p.Reader.List(ctx, list, &client.ListOptions{Namespace: b.Namespace, LabelSelector: query, Limit: 128, Continue: continuation}); err != nil {
				return nil, err
			}
			for _, member := range list.Items {
				if len(member.OwnerReferences) == 1 && member.OwnerReferences[0].Name == name && member.OwnerReferences[0].Kind == "PtahResultRecord" {
					found = append(found, member)
					if len(found) > 128 {
						return nil, ErrRetained
					}
				} else if !legacy {
					return nil, ErrRetained
				}
			}
			continuation = list.Continue
			if continuation == "" {
				break
			}
		}
		if continuation != "" {
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
