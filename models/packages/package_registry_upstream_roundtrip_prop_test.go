// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package packages_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	packages_model "gitea.dev/models/packages"
	"gitea.dev/models/unittest"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// upstreamConfig is the generated shape of one valid upstream configuration. It carries exactly
// the fields named by design Property 1 (type, url, mode, auth type/username/secret, metadata TTL,
// priority, target owner, remote prefix, pinned tags); the remaining columns are provenance /
// observability rather than configuration and are set to fixed values by the property below.
type upstreamConfig struct {
	Type          packages_model.Type
	URL           string
	Mode          packages_model.UpstreamMode
	AuthType      packages_model.UpstreamAuthType
	AuthUsername  string
	AuthSecret    string
	MetadataTTL   int64
	Priority      int64
	TargetOwnerID int64
	RemotePrefix  string
	PinnedTags    []*packages_model.UpstreamPin
}

// upstreamNameSeq makes the generated Name unique per iteration so the (owner_id, type, name)
// unique key cannot collide across the property's runs. The name itself is still round-tripped.
var upstreamNameSeq atomic.Int64

// genPrintableString generates non-empty-capable strings of printable ASCII excluding space, so
// values exercise quoting-relevant characters (quotes, backslash, percent) without depending on
// trailing-whitespace semantics that differ between database backends.
func genPrintableString() gopter.Gen {
	return gen.SliceOf(gen.RuneRange('!', '~')).Map(func(runes []rune) string {
		return string(runes)
	})
}

// genUpstreamConfig composes the configuration generator purely from gopter's gen combinators.
// Only valid configurations are generated: modes and auth types come from the model's enums, TTL
// and priority are positive, and the target owner is an existing fixture user or organization.
func genUpstreamConfig() gopter.Gen {
	// Pinned tags go through the JSON column, so their fields use fully arbitrary strings: JSON
	// escaping is exactly what this part of the round-trip is meant to exercise.
	pinGen := gen.StructPtr(reflect.TypeOf(&packages_model.UpstreamPin{}), map[string]gopter.Gen{
		"Image":  gen.AnyString(),
		"Tag":    gen.AnyString(),
		"Target": gen.AnyString(),
	})

	urlGen := gopter.CombineGens(
		gen.OneConstOf("https", "http"),
		gen.Identifier(),
	).Map(func(vals []any) string {
		return vals[0].(string) + "://" + vals[1].(string) + ".example.test/v2"
	})

	// An empty remote prefix means "request the image path unchanged"; a non-empty one may be a
	// single namespace segment or a nested path.
	remotePrefixGen := gen.Frequency(map[int]gopter.Gen{
		1: gen.Const(""),
		4: gen.Identifier(),
		2: gopter.CombineGens(gen.Identifier(), gen.Identifier()).Map(func(vals []any) string {
			return vals[0].(string) + "/" + vals[1].(string)
		}),
	})

	// Covers the nil list, the empty list (SliceOf yields length 0 when MinSize is 0) and
	// multi-element lists.
	pinnedTagsGen := gen.Frequency(map[int]gopter.Gen{
		1: gen.Const([]*packages_model.UpstreamPin(nil)),
		6: gen.SliceOf(pinGen, reflect.TypeOf(&packages_model.UpstreamPin{})),
	})

	return gen.Struct(reflect.TypeOf(upstreamConfig{}), map[string]gopter.Gen{
		"Type": gen.OneConstOf(
			packages_model.TypeContainer,
			packages_model.TypeMaven,
			packages_model.TypeNpm,
			packages_model.TypePyPI,
		),
		"URL": urlGen,
		"Mode": gen.OneConstOf(
			packages_model.UpstreamModePullThrough,
			packages_model.UpstreamModeFrozen,
		),
		"AuthType": gen.OneConstOf(
			packages_model.UpstreamAuthNone,
			packages_model.UpstreamAuthBasic,
			packages_model.UpstreamAuthToken,
		),
		"AuthUsername": genPrintableString(),
		"AuthSecret":   genPrintableString(),
		"MetadataTTL":  gen.Int64Range(1, 86400),
		"Priority":     gen.Int64Range(1, 1000),
		// Existing fixture owners: users 1, 2, 4, 5 and organizations 3, 6, 7, 17.
		"TargetOwnerID": gen.OneConstOf(int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7), int64(17)),
		"RemotePrefix":  remotePrefixGen,
		"PinnedTags":    pinnedTagsGen,
	})
}

// pinsMismatch compares two pinned-tag lists by value. nil and an empty list are treated as the
// same configuration: emptiness is the configured value, and whether the JSON column materialises
// it as nil or as a zero-length slice is a Go representation detail, not a field value.
func pinsMismatch(want, got []*packages_model.UpstreamPin) string {
	if len(want) != len(got) {
		return fmt.Sprintf("PinnedTags length: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		switch {
		case want[i] == nil && got[i] == nil:
		case want[i] == nil || got[i] == nil:
			return fmt.Sprintf("PinnedTags[%d]: want %v, got %v", i, want[i], got[i])
		case *want[i] != *got[i]:
			return fmt.Sprintf("PinnedTags[%d]: want %+v, got %+v", i, *want[i], *got[i])
		}
	}
	return ""
}

// Feature: proxy-registry-admin-and-orgs, Property 1: Upstream configuration round-trip
//
// For any valid upstream configuration (type, url, mode, auth type/username/secret, metadata TTL,
// priority, target owner, remote prefix, and an arbitrary list of pinned tags), inserting it and
// then reading it back returns equal field values, including the JSON-serialized pinned tags.
//
// The generated configuration sets TargetOwnerID and leaves OwnerID unset, which is the admin
// panel's path through normalizeTargetOwner; the model invariant TargetOwnerID == OwnerID is
// therefore asserted on the value read back rather than generated independently.
//
// **Validates: Requirements 1.3**
func TestUpstreamConfigurationRoundTrip(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 100
	// Bounds generated string and slice sizes so 100 database round-trips stay quick.
	params.MaxSize = 20

	properties := gopter.NewProperties(params)

	properties.Property("an inserted upstream configuration reads back unchanged", prop.ForAll(
		func(cfg upstreamConfig) string {
			name := fmt.Sprintf("prop-upstream-%d", upstreamNameSeq.Add(1))

			inserted, err := packages_model.InsertUpstream(ctx, &packages_model.PackageRegistryUpstream{
				TargetOwnerID:  cfg.TargetOwnerID,
				Type:           cfg.Type,
				Name:           name,
				URL:            cfg.URL,
				Mode:           cfg.Mode,
				AuthType:       cfg.AuthType,
				AuthUsername:   cfg.AuthUsername,
				AuthSecret:     cfg.AuthSecret,
				MetadataTTL:    cfg.MetadataTTL,
				Priority:       cfg.Priority,
				RemotePrefix:   cfg.RemotePrefix,
				PinnedTags:     cfg.PinnedTags,
				Enabled:        true,
				IsAdminManaged: true,
			})
			if err != nil {
				return fmt.Sprintf("InsertUpstream: %v", err)
			}

			got, err := packages_model.GetUpstreamByID(ctx, inserted.ID)
			if err != nil {
				return fmt.Sprintf("GetUpstreamByID(%d): %v", inserted.ID, err)
			}

			var mismatches []string
			check := func(field string, want, have any) {
				if want != have {
					mismatches = append(mismatches, fmt.Sprintf("%s: want %v, got %v", field, want, have))
				}
			}

			check("Type", cfg.Type, got.Type)
			check("Name", name, got.Name)
			check("URL", cfg.URL, got.URL)
			check("Mode", cfg.Mode, got.Mode)
			check("AuthType", cfg.AuthType, got.AuthType)
			check("AuthUsername", cfg.AuthUsername, got.AuthUsername)
			check("AuthSecret", cfg.AuthSecret, got.AuthSecret)
			check("MetadataTTL", cfg.MetadataTTL, got.MetadataTTL)
			check("Priority", cfg.Priority, got.Priority)
			check("TargetOwnerID", cfg.TargetOwnerID, got.TargetOwnerID)
			check("RemotePrefix", cfg.RemotePrefix, got.RemotePrefix)
			check("Enabled", true, got.Enabled)
			check("IsAdminManaged", true, got.IsAdminManaged)
			// The model keeps the target owner and the scoping owner identical (normalizeTargetOwner).
			check("OwnerID", cfg.TargetOwnerID, got.OwnerID)

			if msg := pinsMismatch(cfg.PinnedTags, got.PinnedTags); msg != "" {
				mismatches = append(mismatches, msg)
			}

			return strings.Join(mismatches, "; ")
		},
		genUpstreamConfig(),
	))

	properties.TestingRun(t)
}
