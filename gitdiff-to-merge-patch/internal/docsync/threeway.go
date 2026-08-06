package docsync

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ThreeWay computes the merge patch that brings the document the server
// currently holds in line with the repository, given three inputs rather than
// two.
//
// This is the algorithm `kubectl apply` used client-side, where the three
// inputs were the last-applied-configuration annotation, the manifest being
// applied, and the live object. The mapping onto a Git pipeline is exact:
//
//	base = the document at the merge-base commit  (what we last applied)
//	head = the document at HEAD                   (what we want)
//	live = the document fetched from the API       (what is actually there)
//
// The reason three inputs are needed is that two cannot distinguish the two
// ways a field can be missing:
//
//   - A field in base but not in head was *deleted in Git*, and must be
//     deleted on the server.
//   - A field in live but in neither base nor head was *added by somebody
//     else*, and must be left alone.
//
// Diffing base against head ignores the server entirely and blindly assumes
// it sits at base. Diffing live against head is worse: it cannot tell those
// two cases apart, so it deletes every field the server owns. Only the
// three-way form is correct.
//
// It returns the patch to send and the document that applying that patch
// should produce, so the caller can verify the result the same way as any
// other patch. The expected document is not simply head — it is head plus
// whatever fields the server owns.
//
// Arrays are compared atomically, matching RFC 7396. See the README on
// strategic merge patch for what it would take to do better.
func ThreeWay(base, head, live []byte) (patch, want []byte, err error) {
	b, err := decodeObject("base", base)
	if err != nil {
		return nil, nil, err
	}
	h, err := decodeObject("head", head)
	if err != nil {
		return nil, nil, err
	}
	l, err := decodeObject("live", live)
	if err != nil {
		return nil, nil, err
	}

	patchObj, wantObj := threeWayObject(b, h, l)

	patch, err = json.Marshal(patchObj)
	if err != nil {
		return nil, nil, fmt.Errorf("docsync: encode patch: %w", err)
	}
	want, err = json.Marshal(wantObj)
	if err != nil {
		return nil, nil, fmt.Errorf("docsync: encode expected document: %w", err)
	}
	return patch, want, nil
}

// threeWayObject implements the merge for one object level.
//
// The three rules, in the order they are applied below:
//
//  1. present in head        -> send it if the server disagrees
//  2. in base, gone from head -> delete it (null), because Git removed it
//  3. only in live            -> emit nothing, because we do not own it
func threeWayObject(base, head, live map[string]any) (patch, want map[string]any) {
	patch = map[string]any{}
	// The expected result starts as whatever the server holds: every field we
	// say nothing about survives untouched. That is rule 3, expressed as a
	// default rather than as a branch.
	want = make(map[string]any, len(live))
	for k, v := range live {
		want[k] = v
	}

	// Rule 1.
	for k, hv := range head {
		lv, inLive := live[k]
		hObj, headIsObject := hv.(map[string]any)
		lObj, liveIsObject := lv.(map[string]any)

		// Recurse only when both sides are objects. If the server holds a
		// scalar where the repository holds an object there is nothing to
		// merge into, so the whole subtree is sent.
		if headIsObject && liveIsObject {
			bObj, _ := base[k].(map[string]any)
			subPatch, subWant := threeWayObject(bObj, hObj, lObj)
			if len(subPatch) > 0 {
				patch[k] = subPatch
			}
			want[k] = subWant
			continue
		}

		if !inLive || !equalValue(hv, lv) {
			patch[k] = hv
			want[k] = hv
		}
	}

	// Rule 2. Only fields the server still holds need a deletion; asking it to
	// delete something already absent is noise.
	for k := range base {
		if _, inHead := head[k]; inHead {
			continue
		}
		if _, inLive := live[k]; !inLive {
			continue
		}
		patch[k] = nil
		delete(want, k)
	}

	return patch, want
}

func decodeObject(name string, data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("docsync: decode %s document: %w", name, err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		// A merge patch can only address the members of an object. A document
		// whose root is an array or a scalar has to be replaced wholesale.
		return nil, fmt.Errorf("docsync: %s document root is not an object", name)
	}
	return obj, nil
}

// equalValue compares two decoded values structurally. Marshalling is used
// rather than reflect.DeepEqual because encoding/json sorts object keys, so
// equal documents produce equal bytes regardless of decode order.
func equalValue(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
