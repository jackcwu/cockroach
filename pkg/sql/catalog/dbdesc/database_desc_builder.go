// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package dbdesc

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catprivilege"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/multiregion"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

// DatabaseDescriptorBuilder is an extension of catalog.DescriptorBuilder
// for database descriptors.
type DatabaseDescriptorBuilder interface {
	catalog.DescriptorBuilder
	BuildImmutableDatabase() catalog.DatabaseDescriptor
	BuildExistingMutableDatabase() *Mutable
	BuildCreatedMutableDatabase() *Mutable
}

type databaseDescriptorBuilder struct {
	original      *descpb.DatabaseDescriptor
	maybeModified *descpb.DatabaseDescriptor

	changed bool
}

var _ DatabaseDescriptorBuilder = &databaseDescriptorBuilder{}

// NewBuilder creates a new catalog.DescriptorBuilder object for building
// database descriptors.
func NewBuilder(desc *descpb.DatabaseDescriptor) DatabaseDescriptorBuilder {
	return &databaseDescriptorBuilder{
		original: protoutil.Clone(desc).(*descpb.DatabaseDescriptor),
	}
}

// DescriptorType implements the catalog.DescriptorBuilder interface.
func (ddb *databaseDescriptorBuilder) DescriptorType() catalog.DescriptorType {
	return catalog.Database
}

// RunPostDeserializationChanges implements the catalog.DescriptorBuilder
// interface.
func (ddb *databaseDescriptorBuilder) RunPostDeserializationChanges(
	_ context.Context, _ catalog.DescGetter,
) error {
	ddb.maybeModified = protoutil.Clone(ddb.original).(*descpb.DatabaseDescriptor)

	privsChanged := catprivilege.MaybeFixPrivileges(
		&ddb.maybeModified.Privileges,
		descpb.InvalidID,
		descpb.InvalidID,
		privilege.Database,
		ddb.maybeModified.GetName())
	removedSelfEntryInSchemas := maybeRemoveDroppedSelfEntryFromSchemas(ddb.maybeModified)
	ddb.changed = privsChanged || removedSelfEntryInSchemas
	return nil
}

// RunMigrationChanges implements the catalog.DescriptorBuilder
// interface.
func (ddb *databaseDescriptorBuilder) RunMigrationChanges(
	workFuncs ...catalog.MigrationWorkFunc,
) error {
	for _, applyChange := range workFuncs {
		applyChange(ddb)
	}

	return nil
}

// BuildImmutable implements the catalog.DescriptorBuilder interface.
func (ddb *databaseDescriptorBuilder) BuildImmutable() catalog.Descriptor {
	return ddb.BuildImmutableDatabase()
}

// BuildImmutableDatabase returns an immutable database descriptor.
func (ddb *databaseDescriptorBuilder) BuildImmutableDatabase() catalog.DatabaseDescriptor {
	desc := ddb.maybeModified
	if desc == nil {
		desc = ddb.original
	}
	return &immutable{DatabaseDescriptor: *desc}
}

// BuildExistingMutable implements the catalog.DescriptorBuilder interface.
func (ddb *databaseDescriptorBuilder) BuildExistingMutable() catalog.MutableDescriptor {
	return ddb.BuildExistingMutableDatabase()
}

// BuildExistingMutableDatabase returns a mutable descriptor for a database
// which already exists.
func (ddb *databaseDescriptorBuilder) BuildExistingMutableDatabase() *Mutable {
	if ddb.maybeModified == nil {
		ddb.maybeModified = protoutil.Clone(ddb.original).(*descpb.DatabaseDescriptor)
	}
	return &Mutable{
		immutable:      immutable{DatabaseDescriptor: *ddb.maybeModified},
		ClusterVersion: &immutable{DatabaseDescriptor: *ddb.original},
		changed:        ddb.changed,
	}
}

// BuildCreatedMutable implements the catalog.DescriptorBuilder interface.
func (ddb *databaseDescriptorBuilder) BuildCreatedMutable() catalog.MutableDescriptor {
	return ddb.BuildCreatedMutableDatabase()
}

// BuildCreatedMutableDatabase returns a mutable descriptor for a database
// which is in the process of being created.
func (ddb *databaseDescriptorBuilder) BuildCreatedMutableDatabase() *Mutable {
	desc := ddb.maybeModified
	if desc == nil {
		desc = ddb.original
	}
	return &Mutable{
		immutable: immutable{DatabaseDescriptor: *desc},
		changed:   ddb.changed,
	}
}

// NewInitialOption is an optional argument for NewInitial.
type NewInitialOption func(*descpb.DatabaseDescriptor)

// MaybeWithDatabaseRegionConfig is an option allowing an optional regional
// configuration to be set on the database descriptor.
func MaybeWithDatabaseRegionConfig(regionConfig *multiregion.RegionConfig) NewInitialOption {
	return func(desc *descpb.DatabaseDescriptor) {
		// Not a multi-region database. Not much to do here.
		if regionConfig == nil {
			return
		}
		desc.RegionConfig = &descpb.DatabaseDescriptor_RegionConfig{
			SurvivalGoal:  regionConfig.SurvivalGoal(),
			PrimaryRegion: regionConfig.PrimaryRegion(),
			RegionEnumID:  regionConfig.RegionEnumID(),
			Placement:     regionConfig.Placement(),
		}
	}
}

// WithPublicSchemaID is used to create a DatabaseDescriptor with a
// publicSchemaID.
func WithPublicSchemaID(publicSchemaID descpb.ID) NewInitialOption {
	return func(desc *descpb.DatabaseDescriptor) {
		// TODO(richardjcai): Remove this in 22.2. If the public schema id is
		// keys.PublicSchemaID, we do not add an entry as the public schema does
		// not have a descriptor.
		if publicSchemaID != keys.PublicSchemaID {
			desc.Schemas = map[string]descpb.DatabaseDescriptor_SchemaInfo{
				tree.PublicSchema: {
					ID:      publicSchemaID,
					Dropped: false,
				},
			}
		}
	}
}

// NewInitial constructs a new Mutable for an initial version from an id and
// name with default privileges.
func NewInitial(
	id descpb.ID, name string, owner security.SQLUsername, options ...NewInitialOption,
) *Mutable {
	return newInitialWithPrivileges(
		id,
		name,
		descpb.NewBaseDatabasePrivilegeDescriptor(owner),
		catprivilege.MakeDefaultPrivilegeDescriptor(descpb.DefaultPrivilegeDescriptor_DATABASE),
		options...,
	)
}

// newInitialWithPrivileges constructs a new Mutable for an initial version
// from an id and name and custom privileges.
func newInitialWithPrivileges(
	id descpb.ID,
	name string,
	privileges *descpb.PrivilegeDescriptor,
	defaultPrivileges *descpb.DefaultPrivilegeDescriptor,
	options ...NewInitialOption,
) *Mutable {
	ret := descpb.DatabaseDescriptor{
		Name:              name,
		ID:                id,
		Version:           1,
		Privileges:        privileges,
		DefaultPrivileges: defaultPrivileges,
	}
	for _, option := range options {
		option(&ret)
	}
	return NewBuilder(&ret).BuildCreatedMutableDatabase()
}

// MaybeAddGrantOptionsDatabase is called by maybeAddGrantOptions() in grant_option_migration.go
// and potentially gives grant options to a new database descriptor.
func MaybeAddGrantOptionsDatabase(b DatabaseDescriptorBuilder) {
	ddb := b.(*databaseDescriptorBuilder)
	ddb.maybeModified = protoutil.Clone(ddb.original).(*descpb.DatabaseDescriptor)
	ddb.changed = catprivilege.MaybeUpdateGrantOptions(&ddb.maybeModified.Privileges)
}
