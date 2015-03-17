// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package state

import (
	"fmt"

	"github.com/dustin/go-humanize"
	"github.com/juju/errors"
	"github.com/juju/names"
	jujutxn "github.com/juju/txn"
	"github.com/juju/utils/featureflag"
	"github.com/juju/utils/set"
	"gopkg.in/juju/charm.v4"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"gopkg.in/mgo.v2/txn"

	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/feature"
	"github.com/juju/juju/storage"
	"github.com/juju/juju/storage/poolmanager"
	"github.com/juju/juju/storage/provider"
	"github.com/juju/juju/storage/provider/registry"
)

var ErrPersistentVolumesExist = errors.New(`
Environment cannot be destroyed until all persistent volumes have been destroyed.
Run "juju storage list" to display persistent storage volumes.
`[1:])

// StorageInstance represents the state of a unit or service-wide storage
// instance in the environment.
type StorageInstance interface {
	Entity

	// StorageTag returns the tag for the storage instance.
	StorageTag() names.StorageTag

	// Kind returns the storage instance kind.
	Kind() StorageKind

	// Owner returns the tag of the service or unit that owns this storage
	// instance.
	Owner() names.Tag

	// StorageName returns the name of the storage, as defined in the charm
	// storage metadata. This does not uniquely identify storage instances,
	// but identifies the group that the instances belong to.
	StorageName() string

	// Life reports whether the storage instance is Alive, Dying or Dead.
	Life() Life
}

// StorageAttachment represents the state of a unit's attachment to a storage
// instance. A non-shared storage instance will have a single attachment for
// the storage instance's owning unit, whereas a shared storage instance will
// have an attachment for each unit of the service owning the storage instance.
type StorageAttachment interface {
	// StorageInstance returns the tag of the corresponding storage
	// instance.
	StorageInstance() names.StorageTag

	// Unit returns the tag of the corresponding unit.
	Unit() names.UnitTag

	// Life reports whether the storage attachment is Alive, Dying or Dead.
	Life() Life
}

// StorageKind defines the type of a store: whether it is a block device
// or a filesystem.
type StorageKind int

const (
	StorageKindUnknown StorageKind = iota
	StorageKindBlock
	StorageKindFilesystem
)

type storageInstance struct {
	st  *State
	doc storageInstanceDoc
}

func (s *storageInstance) Tag() names.Tag {
	return s.StorageTag()
}

func (s *storageInstance) StorageTag() names.StorageTag {
	return names.NewStorageTag(s.doc.Id)
}

func (s *storageInstance) Kind() StorageKind {
	return s.doc.Kind
}

func (s *storageInstance) Owner() names.Tag {
	tag, err := names.ParseTag(s.doc.Owner)
	if err != nil {
		// This should be impossible; we do not expose
		// a means of modifying the owner tag.
		panic(err)
	}
	return tag
}

func (s *storageInstance) StorageName() string {
	return s.doc.StorageName
}

func (s *storageInstance) Life() Life {
	return s.doc.Life
}

// storageInstanceDoc describes a charm storage instance.
type storageInstanceDoc struct {
	DocID   string `bson:"_id"`
	EnvUUID string `bson:"env-uuid"`

	Id              string      `bson:"id"`
	Kind            StorageKind `bson:"storagekind"`
	Life            Life        `bson:"life"`
	Owner           string      `bson:"owner"`
	StorageName     string      `bson:"storagename"`
	AttachmentCount int         `bson:"attachmentcount"`
}

type storageAttachment struct {
	doc storageAttachmentDoc
}

func (s *storageAttachment) StorageInstance() names.StorageTag {
	return names.NewStorageTag(s.doc.StorageInstance)
}

func (s *storageAttachment) Unit() names.UnitTag {
	return names.NewUnitTag(s.doc.Unit)
}

func (s *storageAttachment) Life() Life {
	return s.doc.Life
}

// storageAttachmentDoc describes a unit's attachment to a charm storage
// instance.
type storageAttachmentDoc struct {
	DocID   string `bson:"_id"`
	EnvUUID string `bson:"env-uuid"`

	Unit            string `bson:"unitid"`
	StorageInstance string `bson:"storageid"`
	Life            Life   `bson:"life"`
}

// newStorageInstanceId returns a unique storage instance name. The name
// incorporates the storage name as defined in the charm storage metadata,
// and a unique sequence number.
func newStorageInstanceId(st *State, store string) (string, error) {
	seq, err := st.sequence("stores")
	if err != nil {
		return "", errors.Trace(err)
	}
	return fmt.Sprintf("%s/%v", store, seq), nil
}

func storageAttachmentId(unit string, storageInstanceId string) string {
	return fmt.Sprintf("%s#%s", unitGlobalKey(unit), storageInstanceId)
}

// StorageInstance returns the StorageInstance with the specified tag.
func (st *State) StorageInstance(tag names.StorageTag) (StorageInstance, error) {
	s, err := st.storageInstance(tag)
	return s, err
}

func (st *State) storageInstance(tag names.StorageTag) (*storageInstance, error) {
	storageInstances, cleanup := st.getCollection(storageInstancesC)
	defer cleanup()

	s := storageInstance{st: st}
	err := storageInstances.FindId(tag.Id()).One(&s.doc)
	if err == mgo.ErrNotFound {
		return nil, errors.NotFoundf("storage instance %q", tag.Id())
	} else if err != nil {
		return nil, errors.Annotate(err, "cannot get storage instance details")
	}
	return &s, nil
}

// AllStorageInstances lists all storage instances currently in state
// for this Juju environment.
func (st *State) AllStorageInstances() (storageInstances []StorageInstance, err error) {
	storageCollection, closer := st.getCollection(storageInstancesC)
	defer closer()

	sdocs := []storageInstanceDoc{}
	err = storageCollection.Find(nil).All(&sdocs)
	if err != nil {
		return nil, errors.Annotate(err, "cannot get all storage instances")
	}
	for _, doc := range sdocs {
		storageInstances = append(storageInstances, &storageInstance{st, doc})
	}
	return
}

// DestroyStorageInstance ensures that the storage instance and all its
// attachments will be removed at some point; if the storage instance has
// no attachments, it will be removed immediately.
func (st *State) DestroyStorageInstance(tag names.StorageTag) (err error) {
	defer errors.DeferredAnnotatef(&err, "cannot destroy storage %q", tag.Id())
	buildTxn := func(attempt int) ([]txn.Op, error) {
		s, err := st.storageInstance(tag)
		if errors.IsNotFound(err) {
			return nil, jujutxn.ErrNoOperations
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		switch ops, err := st.destroyStorageInstanceOps(s); err {
		case errAlreadyDying:
			return nil, jujutxn.ErrNoOperations
		case nil:
			return ops, nil
		default:
			return nil, errors.Trace(err)
		}
		return nil, jujutxn.ErrTransientFailure
	}
	return st.run(buildTxn)
}

func (st *State) destroyStorageInstanceOps(s *storageInstance) ([]txn.Op, error) {
	if s.doc.Life == Dying {
		return nil, errAlreadyDying
	}
	if s.doc.AttachmentCount == 0 {
		// There are no attachments remaining, so we can
		// remove the storage instance immediately.
		hasNoAttachments := bson.D{{"attachmentcount", 0}}
		assert := append(hasNoAttachments, isAliveDoc...)
		return removeStorageInstanceOps(s.StorageTag(), assert), nil
	}
	// There are still attachments: the storage instance will be removed
	// when the last attachment is removed. We schedule a cleanup to destroy
	// attachments.
	notLastRefs := bson.D{
		{"life", Alive},
		{"attachmentcount", bson.D{{"$gt", 0}}},
	}
	update := bson.D{{"$set", bson.D{{"life", Dying}}}}
	ops := []txn.Op{
		st.newCleanupOp(cleanupAttachmentsForDyingStorage, s.doc.Id),
		{
			C:      storageInstancesC,
			Id:     s.doc.Id,
			Assert: notLastRefs,
			Update: update,
		},
	}
	return ops, nil
}

// removeStorageInstanceOps removes the storage instance with the given
// tag from state, if the specified assertions hold true.
func removeStorageInstanceOps(tag names.StorageTag, assert bson.D) []txn.Op {
	return []txn.Op{{
		C:      storageInstancesC,
		Id:     tag.Id(),
		Assert: assert,
		Remove: true,
	}}
}

// createStorageOps returns txn.Ops for creating storage instances
// and attachments for the newly created unit or service.
//
// The entity tag identifies the entity that owns the storage instance
// either a unit or a service. Shared storage instances are owned by a
// service, and non-shared storage instances are owned by a unit.
//
// The charm metadata corresponds to the charm that the owner (service/unit)
// is or will be running, and is used to extract storage constraints,
// default values, etc.
//
// The supplied storage constraints are constraints for the storage
// instances to be created, keyed on the storage name. These constraints
// will be correlated with the charm storage metadata for validation
// and supplementing.
func createStorageOps(
	st *State,
	entity names.Tag,
	charmMeta *charm.Meta,
	cons map[string]StorageConstraints,
) (ops []txn.Op, numStorageAttachments int, err error) {

	type template struct {
		storageName string
		meta        charm.Storage
		cons        StorageConstraints
	}

	createdShared := false
	switch entity := entity.(type) {
	case names.ServiceTag:
		createdShared = true
	case names.UnitTag:
	default:
		return nil, -1, errors.Errorf("expected service or unit tag, got %T", entity)
	}

	// Create storage instances in order of name, to simplify testing.
	storageNames := set.NewStrings()
	for name := range cons {
		storageNames.Add(name)
	}

	templates := make([]template, 0, len(cons))
	for _, store := range storageNames.SortedValues() {
		cons := cons[store]
		charmStorage, ok := charmMeta.Storage[store]
		if !ok {
			return nil, -1, errors.NotFoundf("charm storage %q", store)
		}
		if createdShared != charmStorage.Shared {
			// services only get shared storage instances,
			// units only get non-shared storage instances.
			continue
		}
		templates = append(templates, template{
			storageName: store,
			meta:        charmStorage,
			cons:        cons,
		})
	}

	ops = make([]txn.Op, 0, len(templates)*2)
	for _, t := range templates {
		owner := entity.String()
		var kind StorageKind
		switch t.meta.Type {
		case charm.StorageBlock:
			kind = StorageKindBlock
		case charm.StorageFilesystem:
			kind = StorageKindFilesystem
		default:
			return nil, -1, errors.Errorf("unknown storage type %q", t.meta.Type)
		}

		for i := uint64(0); i < t.cons.Count; i++ {
			id, err := newStorageInstanceId(st, t.storageName)
			if err != nil {
				return nil, -1, errors.Annotate(err, "cannot generate storage instance name")
			}
			doc := &storageInstanceDoc{
				Id:          id,
				Kind:        kind,
				Owner:       owner,
				StorageName: t.storageName,
			}
			if unit, ok := entity.(names.UnitTag); ok {
				doc.AttachmentCount = 1
				storage := names.NewStorageTag(id)
				ops = append(ops, createStorageAttachmentOp(storage, unit))
				numStorageAttachments++
			}
			ops = append(ops, txn.Op{
				C:      storageInstancesC,
				Id:     id,
				Assert: txn.DocMissing,
				Insert: doc,
			})
		}
	}

	// TODO(axw) create storage attachments for each shared storage
	// instance owned by the service.
	//
	// TODO(axw) prevent creation of shared storage after service
	// creation, because the only sane time to add storage attachments
	// is when units are added to said service.

	return ops, numStorageAttachments, nil
}

// createStorageAttachmentOps returns a txn.Op for creating a storage attachment.
// The caller is responsible for updating the attachmentcount field of the storage
// instance.
func createStorageAttachmentOp(storage names.StorageTag, unit names.UnitTag) txn.Op {
	return txn.Op{
		C:      storageAttachmentsC,
		Id:     storageAttachmentId(unit.Id(), storage.Id()),
		Assert: txn.DocMissing,
		Insert: &storageAttachmentDoc{
			Unit:            unit.Id(),
			StorageInstance: storage.Id(),
		},
	}
}

// StorageAttachments returns the StorageAttachments for the specified unit.
func (st *State) StorageAttachments(unit names.UnitTag) ([]StorageAttachment, error) {
	coll, closer := st.getCollection(storageAttachmentsC)
	defer closer()

	var docs []storageAttachmentDoc
	if err := coll.Find(bson.D{{"unitid", unit.Id()}}).All(&docs); err != nil {
		return nil, errors.Annotatef(err, "cannot get storage attachments for %s", unit.Id())
	}
	storageAttachments := make([]StorageAttachment, len(docs))
	for i, doc := range docs {
		storageAttachments[i] = &storageAttachment{doc}
	}
	return storageAttachments, nil
}

// StorageAttachment returns the StorageAttachment wit hthe specified tags.
func (st *State) StorageAttachment(storage names.StorageTag, unit names.UnitTag) (StorageAttachment, error) {
	att, err := st.storageAttachment(storage, unit)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return att, nil
}

func (st *State) storageAttachment(storage names.StorageTag, unit names.UnitTag) (*storageAttachment, error) {
	coll, closer := st.getCollection(storageAttachmentsC)
	defer closer()
	var s storageAttachment
	err := coll.FindId(storageAttachmentId(unit.Id(), storage.Id())).One(&s.doc)
	if err == mgo.ErrNotFound {
		return nil, errors.NotFoundf("storage attachment %s:%s", storage.Id(), unit.Id())
	} else if err != nil {
		return nil, errors.Annotatef(err, "cannot get storage attachment %s:%s", storage.Id(), unit.Id())
	}
	return &s, nil
}

// DestroyStorageAttachment ensures that the storage attachment will be
// removed at some point.
func (st *State) DestroyStorageAttachment(storage names.StorageTag, unit names.UnitTag) (err error) {
	defer errors.DeferredAnnotatef(&err, "cannot destroy storage attachment %s:%s", storage.Id(), unit.Id())
	buildTxn := func(attempt int) ([]txn.Op, error) {
		s, err := st.storageAttachment(storage, unit)
		if errors.IsNotFound(err) {
			return nil, jujutxn.ErrNoOperations
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		if s.doc.Life == Dying {
			return nil, jujutxn.ErrNoOperations
		}
		return destroyStorageAttachmentOps(s), nil
	}
	return st.run(buildTxn)
}

func destroyStorageAttachmentOps(s *storageAttachment) []txn.Op {
	ops := []txn.Op{{
		C:      storageAttachmentsC,
		Id:     storageAttachmentId(s.doc.Unit, s.doc.StorageInstance),
		Assert: isAliveDoc,
		Update: bson.D{{"$set", bson.D{{"life", Dying}}}},
	}}
	return ops
}

// EnsureStorageAttachmentDead ensures that the storage attachment is Dead
// if it exists at all, doing nothing if it does not exist.
func (st *State) EnsureStorageAttachmentDead(storage names.StorageTag, unit names.UnitTag) (err error) {
	defer errors.DeferredAnnotatef(&err, "cannot ensure death of storage attachment %s:%s", storage.Id(), unit.Id())
	buildTxn := func(attempt int) ([]txn.Op, error) {
		s, err := st.storageAttachment(storage, unit)
		if errors.IsNotFound(err) {
			return nil, jujutxn.ErrNoOperations
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		if s.doc.Life == Dead {
			return nil, jujutxn.ErrNoOperations
		}
		return ensureStorageAttachmentDeadOps(s), nil
	}
	return st.run(buildTxn)
}

func ensureStorageAttachmentDeadOps(s *storageAttachment) []txn.Op {
	ops := []txn.Op{{
		C:      storageAttachmentsC,
		Id:     storageAttachmentId(s.doc.Unit, s.doc.StorageInstance),
		Assert: notDeadDoc,
		Update: bson.D{{"$set", bson.D{{"life", Dead}}}},
	}}
	return ops
}

// Remove removes the storage attachment from state, and may remove its storage
// instance as well, if the storage instance is Dying and no other references to
// it exist. It will fail if the storage attachment is not Dead.
func (st *State) RemoveStorageAttachment(storage names.StorageTag, unit names.UnitTag) (err error) {
	defer errors.DeferredAnnotatef(&err, "cannot remove storage attachment %s:%s", storage.Id(), unit.Id())
	buildTxn := func(attempt int) ([]txn.Op, error) {
		s, err := st.storageAttachment(storage, unit)
		if errors.IsNotFound(err) {
			return nil, jujutxn.ErrNoOperations
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		inst, err := st.storageInstance(storage)
		if errors.IsNotFound(err) {
			// This implies that the attachment was removed
			// after the call to st.storageAttachment.
			return nil, jujutxn.ErrNoOperations
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		ops, err := removeStorageAttachmentOps(s, inst)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return ops, nil
	}
	return st.run(buildTxn)
}

func removeStorageAttachmentOps(s *storageAttachment, si *storageInstance) ([]txn.Op, error) {
	if s.doc.Life != Dead {
		return nil, errors.New("storage attachment is not dead")
	}
	ops := []txn.Op{{
		C:      storageAttachmentsC,
		Id:     storageAttachmentId(s.doc.Unit, s.doc.StorageInstance),
		Assert: isDeadDoc,
		Remove: true,
	}, {
		C:      unitsC,
		Id:     s.doc.Unit,
		Assert: txn.DocExists,
		Update: bson.D{{"$inc", bson.D{{"storageattachmentcount", -1}}}},
	}}
	if si.doc.Life == Dying && si.doc.AttachmentCount == 1 {
		hasLastRef := bson.D{{"life", Dying}, {"attachmentcount", 1}}
		ops = append(ops, removeStorageInstanceOps(si.StorageTag(), hasLastRef)...)
		return ops, nil
	}
	decrefOp := txn.Op{
		C:      storageInstancesC,
		Id:     si.doc.Id,
		Update: bson.D{{"$inc", bson.D{{"attachmentcount", -1}}}},
	}
	if si.doc.Life == Alive {
		// This may be the last reference, but the storage instance is
		// still alive. The storage instance will be removed when its
		// Destroy method is called, if it has no attachments.
		decrefOp.Assert = bson.D{
			{"life", Alive},
			{"attachmentcount", bson.D{{"$gt", 0}}},
		}
	} else {
		// If it's not the last reference when we checked, we want to
		// allow for concurrent attachment removals but want to ensure
		// that we don't drop to zero without removing the storage
		// instance.
		decrefOp.Assert = bson.D{
			{"life", Dying},
			{"attachmentcount", bson.D{{"$gt", 1}}},
		}
	}
	ops = append(ops, decrefOp)
	return ops, nil
}

// removeStorageInstancesOps returns the transaction operations to remove all
// storage instances owned by the specified entity.
func removeStorageInstancesOps(st *State, owner names.Tag) ([]txn.Op, error) {
	coll, closer := st.getCollection(storageInstancesC)
	defer closer()

	var docs []storageInstanceDoc
	err := coll.Find(bson.D{{"owner", owner.String()}}).Select(bson.D{{"id", true}}).All(&docs)
	if err != nil {
		return nil, errors.Annotatef(err, "cannot get storage instances for %s", owner)
	}
	ops := make([]txn.Op, len(docs))
	for i, doc := range docs {
		ops[i] = txn.Op{
			C:      storageInstancesC,
			Id:     doc.Id,
			Remove: true,
		}
	}
	return ops, nil
}

// storageConstraintsDoc contains storage constraints for an entity.
type storageConstraintsDoc struct {
	DocID       string                        `bson:"_id"`
	EnvUUID     string                        `bson:"env-uuid"`
	Constraints map[string]StorageConstraints `bson:"constraints"`
}

// StorageConstraints contains the user-specified constraints for provisioning
// storage instances for a service unit.
type StorageConstraints struct {
	// Pool is the name of the storage pool from which to provision the
	// storage instances.
	Pool string `bson:"pool"`

	// Size is the required size of the storage instances, in MiB.
	Size uint64 `bson:"size"`

	// Count is the required number of storage instances.
	Count uint64 `bson:"count"`
}

func createStorageConstraintsOp(key string, cons map[string]StorageConstraints) txn.Op {
	return txn.Op{
		C:      storageConstraintsC,
		Id:     key,
		Assert: txn.DocMissing,
		Insert: &storageConstraintsDoc{
			Constraints: cons,
		},
	}
}

func removeStorageConstraintsOp(key string) txn.Op {
	return txn.Op{
		C:      storageConstraintsC,
		Id:     key,
		Remove: true,
	}
}

func readStorageConstraints(st *State, key string) (map[string]StorageConstraints, error) {
	coll, closer := st.getCollection(storageConstraintsC)
	defer closer()

	var doc storageConstraintsDoc
	err := coll.FindId(key).One(&doc)
	if err == mgo.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Annotatef(err, "cannot get storage constraints for %q", key)
	}
	return doc.Constraints, nil
}

func storageKind(storageType charm.StorageType) storage.StorageKind {
	kind := storage.StorageKindUnknown
	switch storageType {
	case charm.StorageBlock:
		kind = storage.StorageKindBlock
	case charm.StorageFilesystem:
		kind = storage.StorageKindFilesystem
	}
	return kind
}

func validateStorageConstraints(st *State, allCons map[string]StorageConstraints, charmMeta *charm.Meta) error {
	// TODO(axw) stop checking feature flag once storage has graduated.
	if !featureflag.Enabled(feature.Storage) {
		return nil
	}
	for name, cons := range allCons {
		charmStorage, ok := charmMeta.Storage[name]
		if !ok {
			return errors.Errorf("charm %q has no store called %q", charmMeta.Name, name)
		}
		if charmStorage.Shared {
			// TODO(axw) implement shared storage support.
			return errors.Errorf(
				"charm %q store %q: shared storage support not implemented",
				charmMeta.Name, name,
			)
		}
		if cons.Count < uint64(charmStorage.CountMin) {
			return errors.Errorf(
				"charm %q store %q: %d instances required, %d specified",
				charmMeta.Name, name, charmStorage.CountMin, cons.Count,
			)
		}
		if charmStorage.CountMax >= 0 && cons.Count > uint64(charmStorage.CountMax) {
			return errors.Errorf(
				"charm %q store %q: at most %d instances supported, %d specified",
				charmMeta.Name, name, charmStorage.CountMax, cons.Count,
			)
		}
		if charmStorage.MinimumSize > 0 && cons.Size < charmStorage.MinimumSize {
			return errors.Errorf(
				"charm %q store %q: minimum storage size is %s, %s specified",
				charmMeta.Name, name, humanize.Bytes(charmStorage.MinimumSize*humanize.MByte), humanize.Bytes(cons.Size*humanize.MByte),
			)
		}
		kind := storageKind(charmStorage.Type)
		if err := validateStoragePool(st, cons.Pool, kind, nil); err != nil {
			return err
		}
	}
	// Ensure all stores have constraints specified. Defaults should have
	// been set by this point, if the user didn't specify constraints.
	for name, charmStorage := range charmMeta.Storage {
		if _, ok := allCons[name]; !ok && charmStorage.CountMin > 0 {
			return errors.Errorf("no constraints specified for store %q", name)
		}
	}
	return nil
}

// validateStoragePool validates the storage pool for the environment.
// If machineId is non-nil, the storage scope will be validated against
// the machineId; if the storage is not machine-scoped, then the machineId
// will be updated to "".
func validateStoragePool(
	st *State, poolName string, kind storage.StorageKind, machineId *string,
) error {
	if poolName == "" {
		return errors.New("pool name is required")
	}
	providerType, provider, err := poolStorageProvider(st, poolName)
	if err != nil {
		return errors.Trace(err)
	}

	// Ensure the storage provider supports the specified kind.
	var kindSupported bool
	switch kind {
	case storage.StorageKindFilesystem:
		// Filesystems can be created if either filesystem or block
		// storage are supported.
		if provider.Supports(storage.StorageKindBlock) {
			kindSupported = true
			break
		}
		fallthrough
	default:
		kindSupported = provider.Supports(kind)
	}
	if !kindSupported {
		return errors.Errorf("%q provider does not support %q storage", providerType, kind)
	}

	// Check the storage scope.
	if machineId != nil {
		switch provider.Scope() {
		case storage.ScopeMachine:
			if *machineId == "" {
				return errors.Annotate(err, "machine unspecified for machine-scoped storage")
			}
		default:
			// The storage is not machine-scoped, so we clear out
			// the machine ID to inform the caller that the storage
			// scope should be the environment.
			*machineId = ""
		}
	}

	// Ensure the pool type is supported by the environment.
	conf, err := st.EnvironConfig()
	if err != nil {
		return errors.Trace(err)
	}
	envType := conf.Type()
	if !registry.IsProviderSupported(envType, providerType) {
		return errors.Errorf(
			"pool %q uses storage provider %q which is not supported for environments of type %q",
			poolName,
			providerType,
			envType,
		)
	}
	return nil
}

func poolStorageProvider(st *State, poolName string) (storage.ProviderType, storage.Provider, error) {
	poolManager := poolmanager.New(NewStateSettings(st))
	pool, err := poolManager.Get(poolName)
	if errors.IsNotFound(err) {
		// If there's no pool called poolName, maybe a provider type
		// has been specified directly.
		providerType := storage.ProviderType(poolName)
		provider, err1 := registry.StorageProvider(providerType)
		if err1 != nil {
			// The name can't be resolved as a storage provider type,
			// so return the original "pool not found" error.
			return "", nil, errors.Trace(err)
		}
		return providerType, provider, nil
	} else if err != nil {
		return "", nil, errors.Trace(err)
	}
	providerType := pool.Provider()
	provider, err := registry.StorageProvider(providerType)
	if err != nil {
		return "", nil, errors.Trace(err)
	}
	return providerType, provider, nil
}

// ErrNoDefaultStoragePool is returned when a storage pool is required but none
// is specified nor available as a default.
var ErrNoDefaultStoragePool = fmt.Errorf("no storage pool specifed and no default available")

// addDefaultStorageConstraints fills in default constraint values, replacing any empty/missing values
// in the specified constraints.
func addDefaultStorageConstraints(st *State, allCons map[string]StorageConstraints, charmMeta *charm.Meta) error {
	// TODO(axw) stop checking feature flag once storage has graduated.
	if !featureflag.Enabled(feature.Storage) {
		return nil
	}
	conf, err := st.EnvironConfig()
	if err != nil {
		return errors.Trace(err)
	}

	if allCons == nil {
		allCons = make(map[string]StorageConstraints)
	}
	for name, charmStorage := range charmMeta.Storage {
		cons := allCons[name]
		kind := storageKind(charmStorage.Type)
		var err error
		cons, err = storageConstraintsWithDefaults(conf, kind, charmStorage, cons)
		if err != nil {
			if err == ErrNoDefaultStoragePool {
				err = errors.Maskf(err, "no storage pool specified and no default available for %q storage", name)
			}
			return err
		}
		// Replace in case pool or size were updated.
		allCons[name] = cons
	}
	return nil
}

// storageConstraintsWithDefaults returns a constraints derived from cons, with any defaults filled in.
func storageConstraintsWithDefaults(cfg *config.Config, kind storage.StorageKind,
	charmStorage charm.Storage, cons StorageConstraints,
) (StorageConstraints, error) {
	withDefaults := cons
	if cons.Pool == "" {
		poolName, err := defaultStoragePool(cfg, kind)
		if err != nil {
			return withDefaults, errors.Annotatef(err, "finding default stoage pool")
		}
		withDefaults.Pool = poolName
	}
	if cons.Size == 0 {
		if charmStorage.MinimumSize > 0 {
			withDefaults.Size = charmStorage.MinimumSize
		} else {
			withDefaults.Size = 1024
		}
	}
	if cons.Count == 0 {
		withDefaults.Count = uint64(charmStorage.CountMin)
	}
	return withDefaults, nil
}

// defaultStoragePool returns the default storage pool for the environment.
// The default pool is either user specified, or one that is registered by the provider itself.
func defaultStoragePool(cfg *config.Config, kind storage.StorageKind) (string, error) {
	switch kind {
	case storage.StorageKindBlock:
		defaultPool, ok := cfg.StorageDefaultBlockSource()
		if !ok {
			defaultPool = string(provider.LoopProviderType)
		}
		return defaultPool, nil
	}
	return "", ErrNoDefaultStoragePool
}
