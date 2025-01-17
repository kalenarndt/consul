package state

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/go-memdb"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/configentry"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/lib/maps"
	"github.com/hashicorp/consul/proto/pbpeering"
)

const (
	tablePeering             = "peering"
	tablePeeringTrustBundles = "peering-trust-bundles"
)

func peeringTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: tablePeering,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: indexerSingle[string, *pbpeering.Peering]{
					readIndex:  indexFromUUIDString,
					writeIndex: indexIDFromPeering,
				},
			},
			indexName: {
				Name:         indexName,
				AllowMissing: false,
				Unique:       true,
				Indexer: indexerSingleWithPrefix[Query, *pbpeering.Peering, any]{
					readIndex:   indexPeeringFromQuery,
					writeIndex:  indexFromPeering,
					prefixIndex: prefixIndexFromQueryNoNamespace,
				},
			},
			indexDeleted: {
				Name:         indexDeleted,
				AllowMissing: false,
				Unique:       false,
				Indexer: indexerSingle[BoolQuery, *pbpeering.Peering]{
					readIndex:  indexDeletedFromBoolQuery,
					writeIndex: indexDeletedFromPeering,
				},
			},
		},
	}
}

func peeringTrustBundlesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: tablePeeringTrustBundles,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: indexerSingleWithPrefix[Query, *pbpeering.PeeringTrustBundle, any]{
					readIndex:   indexPeeringFromQuery, // same as peering table since we'll use the query.Value
					writeIndex:  indexFromPeeringTrustBundle,
					prefixIndex: prefixIndexFromQueryNoNamespace,
				},
			},
		},
	}
}

func indexIDFromPeering(p *pbpeering.Peering) ([]byte, error) {
	if p.ID == "" {
		return nil, errMissingValueForIndex
	}

	uuid, err := uuidStringToBytes(p.ID)
	if err != nil {
		return nil, err
	}
	var b indexBuilder
	b.Raw(uuid)
	return b.Bytes(), nil
}

func indexDeletedFromPeering(p *pbpeering.Peering) ([]byte, error) {
	var b indexBuilder
	b.Bool(!p.IsActive())
	return b.Bytes(), nil
}

func (s *Store) PeeringReadByID(ws memdb.WatchSet, id string) (uint64, *pbpeering.Peering, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	peering, err := peeringReadByIDTxn(tx, ws, id)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to read peering by id: %w", err)
	}
	if peering == nil {
		// Return the tables index so caller can watch it for changes if the peering doesn't exist
		return maxIndexWatchTxn(tx, ws, tablePeering), nil, nil
	}

	return peering.ModifyIndex, peering, nil
}

func peeringReadByIDTxn(tx ReadTxn, ws memdb.WatchSet, id string) (*pbpeering.Peering, error) {
	watchCh, peeringRaw, err := tx.FirstWatch(tablePeering, indexID, id)
	if err != nil {
		return nil, fmt.Errorf("failed peering lookup: %w", err)
	}
	ws.Add(watchCh)

	peering, ok := peeringRaw.(*pbpeering.Peering)
	if peeringRaw != nil && !ok {
		return nil, fmt.Errorf("invalid type %T", peering)
	}
	return peering, nil
}

func (s *Store) PeeringRead(ws memdb.WatchSet, q Query) (uint64, *pbpeering.Peering, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	return peeringReadTxn(tx, ws, q)
}

func peeringReadTxn(tx ReadTxn, ws memdb.WatchSet, q Query) (uint64, *pbpeering.Peering, error) {
	watchCh, peeringRaw, err := tx.FirstWatch(tablePeering, indexName, q)
	if err != nil {
		return 0, nil, fmt.Errorf("failed peering lookup: %w", err)
	}

	peering, ok := peeringRaw.(*pbpeering.Peering)
	if peeringRaw != nil && !ok {
		return 0, nil, fmt.Errorf("invalid type %T", peering)
	}
	ws.Add(watchCh)

	if peering == nil {
		// Return the tables index so caller can watch it for changes if the peering doesn't exist
		return maxIndexWatchTxn(tx, ws, partitionedIndexEntryName(tablePeering, q.PartitionOrDefault())), nil, nil
	}

	return peering.ModifyIndex, peering, nil
}

func (s *Store) PeeringList(ws memdb.WatchSet, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.Peering, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()
	return peeringListTxn(ws, tx, entMeta)
}

func peeringListTxn(ws memdb.WatchSet, tx ReadTxn, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.Peering, error) {
	var (
		iter memdb.ResultIterator
		err  error
		idx  uint64
	)
	if entMeta.PartitionOrDefault() == structs.WildcardSpecifier {
		iter, err = tx.Get(tablePeering, indexID)
		idx = maxIndexWatchTxn(tx, ws, tablePeering)
	} else {
		iter, err = tx.Get(tablePeering, indexName+"_prefix", entMeta)
		idx = maxIndexWatchTxn(tx, ws, partitionedIndexEntryName(tablePeering, entMeta.PartitionOrDefault()))
	}
	if err != nil {
		return 0, nil, fmt.Errorf("failed peering lookup: %v", err)
	}

	var result []*pbpeering.Peering
	for entry := iter.Next(); entry != nil; entry = iter.Next() {
		result = append(result, entry.(*pbpeering.Peering))
	}

	return idx, result, nil
}

func (s *Store) PeeringWrite(idx uint64, p *pbpeering.Peering) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Check that the ID and Name are set.
	if p.ID == "" {
		return errors.New("Missing Peering ID")
	}
	if p.Name == "" {
		return errors.New("Missing Peering Name")
	}

	// ensure the name is unique (cannot conflict with another peering with a different ID)
	_, existing, err := peeringReadTxn(tx, nil, Query{
		Value:          p.Name,
		EnterpriseMeta: *structs.NodeEnterpriseMetaInPartition(p.Partition),
	})
	if err != nil {
		return err
	}

	if existing != nil {
		if p.ID != existing.ID {
			return fmt.Errorf("A peering already exists with the name %q and a different ID %q", p.Name, existing.ID)
		}
		// Prevent modifications to Peering marked for deletion
		if !existing.IsActive() {
			return fmt.Errorf("cannot write to peering that is marked for deletion")
		}

		if p.State == pbpeering.PeeringState_UNDEFINED {
			p.State = existing.State
		}
		// TODO(peering): Confirm behavior when /peering/token is called more than once.
		// We may need to avoid clobbering existing values.
		p.ImportedServiceCount = existing.ImportedServiceCount
		p.ExportedServiceCount = existing.ExportedServiceCount
		p.CreateIndex = existing.CreateIndex
		p.ModifyIndex = idx
	} else {
		idMatch, err := peeringReadByIDTxn(tx, nil, p.ID)
		if err != nil {
			return err
		}
		if idMatch != nil {
			return fmt.Errorf("A peering already exists with the ID %q and a different name %q", p.Name, existing.ID)
		}

		if !p.IsActive() {
			return fmt.Errorf("cannot create a new peering marked for deletion")
		}
		if p.State == 0 {
			p.State = pbpeering.PeeringState_PENDING
		}
		p.CreateIndex = idx
		p.ModifyIndex = idx
	}

	if err := tx.Insert(tablePeering, p); err != nil {
		return fmt.Errorf("failed inserting peering: %w", err)
	}

	if err := updatePeeringTableIndexes(tx, idx, p.PartitionOrDefault()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PeeringDelete(idx uint64, q Query) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	existing, err := tx.First(tablePeering, indexName, q)
	if err != nil {
		return fmt.Errorf("failed peering lookup: %v", err)
	}

	if existing == nil {
		return nil
	}

	if existing.(*pbpeering.Peering).IsActive() {
		return fmt.Errorf("cannot delete a peering without first marking for deletion")
	}

	if err := tx.Delete(tablePeering, existing); err != nil {
		return fmt.Errorf("failed deleting peering: %v", err)
	}

	if err := updatePeeringTableIndexes(tx, idx, q.PartitionOrDefault()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PeeringTerminateByID(idx uint64, id string) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	existing, err := peeringReadByIDTxn(tx, nil, id)
	if err != nil {
		return fmt.Errorf("failed to read peering %q: %w", id, err)
	}
	if existing == nil {
		return nil
	}

	c := proto.Clone(existing)
	clone, ok := c.(*pbpeering.Peering)
	if !ok {
		return fmt.Errorf("invalid type %T, expected *pbpeering.Peering", existing)
	}

	clone.State = pbpeering.PeeringState_TERMINATED
	clone.ModifyIndex = idx

	if err := tx.Insert(tablePeering, clone); err != nil {
		return fmt.Errorf("failed inserting peering: %w", err)
	}

	if err := updatePeeringTableIndexes(tx, idx, clone.PartitionOrDefault()); err != nil {
		return err
	}
	return tx.Commit()
}

// ExportedServicesForPeer returns the list of typical and proxy services
// exported to a peer.
//
// TODO(peering): What to do about terminating gateways? Sometimes terminating
// gateways are the appropriate destination to dial for an upstream mesh
// service. However, that information is handled by observing the terminating
// gateway's config entry, which we wouldn't want to replicate. How would
// client peers know to route through terminating gateways when they're not
// dialing through a remote mesh gateway?
func (s *Store) ExportedServicesForPeer(ws memdb.WatchSet, peerID string, dc string) (uint64, *structs.ExportedServiceList, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	peering, err := peeringReadByIDTxn(tx, ws, peerID)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to read peering: %w", err)
	}
	if peering == nil {
		return 0, &structs.ExportedServiceList{}, nil
	}

	return exportedServicesForPeerTxn(ws, tx, peering, dc)
}

func (s *Store) ExportedServicesForAllPeersByName(ws memdb.WatchSet, entMeta acl.EnterpriseMeta) (uint64, map[string]structs.ServiceList, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	maxIdx, peerings, err := peeringListTxn(ws, tx, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to list peerings: %w", err)
	}

	out := make(map[string]structs.ServiceList)
	for _, peering := range peerings {
		idx, list, err := exportedServicesForPeerTxn(ws, tx, peering, "")
		if err != nil {
			return 0, nil, fmt.Errorf("failed to list exported services for peer %q: %w", peering.ID, err)
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		m := list.ListAllDiscoveryChains()
		if len(m) > 0 {
			out[peering.Name] = maps.SliceOfKeys(m)
		}
	}

	return maxIdx, out, nil
}

// exportedServicesForPeerTxn will find all services that are exported to a
// specific peering, and optionally include information about discovery chain
// reachable targets for these exported services if the "dc" parameter is
// specified.
func exportedServicesForPeerTxn(
	ws memdb.WatchSet,
	tx ReadTxn,
	peering *pbpeering.Peering,
	dc string,
) (uint64, *structs.ExportedServiceList, error) {
	maxIdx := peering.ModifyIndex

	entMeta := structs.NodeEnterpriseMetaInPartition(peering.Partition)
	idx, conf, err := getExportedServicesConfigEntryTxn(tx, ws, nil, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to fetch exported-services config entry: %w", err)
	}
	if idx > maxIdx {
		maxIdx = idx
	}
	if conf == nil {
		return maxIdx, &structs.ExportedServiceList{}, nil
	}

	var (
		normalSet = make(map[structs.ServiceName]struct{})
		discoSet  = make(map[structs.ServiceName]struct{})
	)

	// At least one of the following should be true for a name for it to
	// replicate:
	//
	// - are a discovery chain by definition (service-router, service-splitter, service-resolver)
	// - have an explicit sidecar kind=connect-proxy
	// - use connect native mode

	for _, svc := range conf.Services {
		svcMeta := acl.NewEnterpriseMetaWithPartition(entMeta.PartitionOrDefault(), svc.Namespace)

		sawPeer := false
		for _, consumer := range svc.Consumers {
			name := structs.NewServiceName(svc.Name, &svcMeta)

			if _, ok := normalSet[name]; ok {
				// Service was covered by a wildcard that was already accounted for
				continue
			}
			if consumer.PeerName != peering.Name {
				continue
			}
			sawPeer = true

			if svc.Name != structs.WildcardSpecifier {
				normalSet[name] = struct{}{}
			}
		}

		// If the target peer is a consumer, and all services in the namespace are exported, query those service names.
		if sawPeer && svc.Name == structs.WildcardSpecifier {
			idx, typicalServices, err := serviceNamesOfKindTxn(tx, ws, structs.ServiceKindTypical, svcMeta)
			if err != nil {
				return 0, nil, fmt.Errorf("failed to get typical service names: %w", err)
			}
			if idx > maxIdx {
				maxIdx = idx
			}
			for _, s := range typicalServices {
				normalSet[s.Service] = struct{}{}
			}

			// list all config entries of kind service-resolver, service-router, service-splitter?
			idx, discoChains, err := listDiscoveryChainNamesTxn(tx, ws, nil, svcMeta)
			if err != nil {
				return 0, nil, fmt.Errorf("failed to get discovery chain names: %w", err)
			}
			if idx > maxIdx {
				maxIdx = idx
			}
			for _, sn := range discoChains {
				discoSet[sn] = struct{}{}
			}
		}
	}

	normal := maps.SliceOfKeys(normalSet)
	disco := maps.SliceOfKeys(discoSet)

	chainInfo := make(map[structs.ServiceName]structs.ExportedDiscoveryChainInfo)
	populateChainInfo := func(svc structs.ServiceName) error {
		if _, ok := chainInfo[svc]; ok {
			return nil // already processed
		}

		var info structs.ExportedDiscoveryChainInfo

		idx, protocol, err := protocolForService(tx, ws, svc)
		if err != nil {
			return fmt.Errorf("failed to get protocol for service %q: %w", svc, err)
		}

		if idx > maxIdx {
			maxIdx = idx
		}
		info.Protocol = protocol

		if dc != "" && !structs.IsProtocolHTTPLike(protocol) {
			// We only need to populate the targets for replication purposes for L4 protocols, which
			// do not ultimately get intercepted by the mesh gateways.
			idx, targets, err := discoveryChainOriginalTargetsTxn(tx, ws, dc, svc.Name, &svc.EnterpriseMeta)
			if err != nil {
				return fmt.Errorf("failed to get discovery chain targets for service %q: %w", svc, err)
			}

			if idx > maxIdx {
				maxIdx = idx
			}

			sort.Slice(targets, func(i, j int) bool {
				return targets[i].ID < targets[j].ID
			})

			info.TCPTargets = targets
		}

		chainInfo[svc] = info
		return nil
	}

	for _, svc := range normal {
		if err := populateChainInfo(svc); err != nil {
			return 0, nil, err
		}
	}
	for _, svc := range disco {
		if err := populateChainInfo(svc); err != nil {
			return 0, nil, err
		}
	}

	structs.ServiceList(normal).Sort()

	list := &structs.ExportedServiceList{
		Services:    normal,
		DiscoChains: chainInfo,
	}

	return maxIdx, list, nil
}

func listAllExportedServices(
	ws memdb.WatchSet,
	tx ReadTxn,
	overrides map[configentry.KindName]structs.ConfigEntry,
	entMeta acl.EnterpriseMeta,
) (uint64, map[structs.ServiceName]struct{}, error) {
	idx, export, err := getExportedServicesConfigEntryTxn(tx, ws, overrides, &entMeta)
	if err != nil {
		return 0, nil, err
	}

	found := make(map[structs.ServiceName]struct{})
	if export == nil {
		return idx, found, nil
	}

	_, services, err := listServicesExportedToAnyPeerByConfigEntry(ws, tx, export, overrides)
	if err != nil {
		return 0, nil, err
	}
	for _, svc := range services {
		found[svc] = struct{}{}
	}

	return idx, found, nil
}

func listServicesExportedToAnyPeerByConfigEntry(
	ws memdb.WatchSet,
	tx ReadTxn,
	conf *structs.ExportedServicesConfigEntry,
	overrides map[configentry.KindName]structs.ConfigEntry,
) (uint64, []structs.ServiceName, error) {
	var (
		entMeta = conf.GetEnterpriseMeta()
		found   = make(map[structs.ServiceName]struct{})
		maxIdx  uint64
	)

	for _, svc := range conf.Services {
		svcMeta := acl.NewEnterpriseMetaWithPartition(entMeta.PartitionOrDefault(), svc.Namespace)

		sawPeer := false
		for _, consumer := range svc.Consumers {
			if consumer.PeerName == "" {
				continue
			}
			sawPeer = true

			sn := structs.NewServiceName(svc.Name, &svcMeta)
			if _, ok := found[sn]; ok {
				continue
			}

			if svc.Name != structs.WildcardSpecifier {
				found[sn] = struct{}{}
			}
		}

		if sawPeer && svc.Name == structs.WildcardSpecifier {
			idx, discoChains, err := listDiscoveryChainNamesTxn(tx, ws, overrides, svcMeta)
			if err != nil {
				return 0, nil, fmt.Errorf("failed to get discovery chain names: %w", err)
			}
			if idx > maxIdx {
				maxIdx = idx
			}
			for _, sn := range discoChains {
				found[sn] = struct{}{}
			}
		}
	}

	foundKeys := maps.SliceOfKeys(found)

	structs.ServiceList(foundKeys).Sort()

	return maxIdx, foundKeys, nil
}

// PeeringsForService returns the list of peerings that are associated with the service name provided in the query.
// This is used to configure connect proxies for a given service. The result is generated by querying for exported
// service config entries and filtering for those that match the given service.
//
// TODO(peering): this implementation does all of the work on read to materialize this list of peerings, we should explore
// writing to a separate index that has service peerings prepared ahead of time should this become a performance bottleneck.
func (s *Store) PeeringsForService(ws memdb.WatchSet, serviceName string, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.Peering, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	return peeringsForServiceTxn(tx, ws, serviceName, entMeta)
}

func peeringsForServiceTxn(tx ReadTxn, ws memdb.WatchSet, serviceName string, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.Peering, error) {
	// Return the idx of the config entry so the caller can watch for changes.
	maxIdx, peerNames, err := peersForServiceTxn(tx, ws, serviceName, &entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to read peers for service name %q: %w", serviceName, err)
	}

	var peerings []*pbpeering.Peering

	// Lookup and return the peering corresponding to each name.
	for _, name := range peerNames {
		readQuery := Query{
			Value:          name,
			EnterpriseMeta: *structs.NodeEnterpriseMetaInPartition(entMeta.PartitionOrDefault()),
		}
		idx, peering, err := peeringReadTxn(tx, ws, readQuery)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to read peering: %w", err)
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		if peering == nil || !peering.IsActive() {
			continue
		}
		peerings = append(peerings, peering)
	}
	return maxIdx, peerings, nil
}

// TrustBundleListByService returns the trust bundles for all peers that the
// given service is exported to, via a discovery chain target.
func (s *Store) TrustBundleListByService(ws memdb.WatchSet, service, dc string, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.PeeringTrustBundle, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	realSvc := structs.NewServiceName(service, &entMeta)

	maxIdx, chainNames, err := s.discoveryChainSourcesTxn(tx, ws, dc, realSvc)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to list all discovery chains referring to %q: %w", realSvc, err)
	}

	peerNames := make(map[string]struct{})
	for _, chainSvc := range chainNames {
		idx, peers, err := peeringsForServiceTxn(tx, ws, chainSvc.Name, chainSvc.EnterpriseMeta)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to get peers for service %s: %v", chainSvc, err)
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		for _, peer := range peers {
			peerNames[peer.Name] = struct{}{}
		}
	}
	peerNamesSlice := maps.SliceOfKeys(peerNames)
	sort.Strings(peerNamesSlice)

	var resp []*pbpeering.PeeringTrustBundle
	for _, peerName := range peerNamesSlice {
		pq := Query{
			Value:          strings.ToLower(peerName),
			EnterpriseMeta: *structs.NodeEnterpriseMetaInPartition(entMeta.PartitionOrDefault()),
		}
		idx, trustBundle, err := peeringTrustBundleReadTxn(tx, ws, pq)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to read trust bundle for peer %s: %v", peerName, err)
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		if trustBundle != nil {
			resp = append(resp, trustBundle)
		}
	}

	return maxIdx, resp, nil
}

// PeeringTrustBundleList returns the peering trust bundles for all peers.
func (s *Store) PeeringTrustBundleList(ws memdb.WatchSet, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.PeeringTrustBundle, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	return peeringTrustBundleListTxn(tx, ws, entMeta)
}

func peeringTrustBundleListTxn(tx ReadTxn, ws memdb.WatchSet, entMeta acl.EnterpriseMeta) (uint64, []*pbpeering.PeeringTrustBundle, error) {
	iter, err := tx.Get(tablePeeringTrustBundles, indexID+"_prefix", entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed peering trust bundle lookup: %w", err)
	}

	idx := maxIndexWatchTxn(tx, ws, partitionedIndexEntryName(tablePeeringTrustBundles, entMeta.PartitionOrDefault()))

	var result []*pbpeering.PeeringTrustBundle
	for entry := iter.Next(); entry != nil; entry = iter.Next() {
		result = append(result, entry.(*pbpeering.PeeringTrustBundle))
	}

	return idx, result, nil
}

// PeeringTrustBundleRead returns the peering trust bundle for the peer name given as the query value.
func (s *Store) PeeringTrustBundleRead(ws memdb.WatchSet, q Query) (uint64, *pbpeering.PeeringTrustBundle, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	return peeringTrustBundleReadTxn(tx, ws, q)
}

func peeringTrustBundleReadTxn(tx ReadTxn, ws memdb.WatchSet, q Query) (uint64, *pbpeering.PeeringTrustBundle, error) {
	watchCh, ptbRaw, err := tx.FirstWatch(tablePeeringTrustBundles, indexID, q)
	if err != nil {
		return 0, nil, fmt.Errorf("failed peering trust bundle lookup: %w", err)
	}

	ptb, ok := ptbRaw.(*pbpeering.PeeringTrustBundle)
	if ptbRaw != nil && !ok {
		return 0, nil, fmt.Errorf("invalid type %T", ptb)
	}
	ws.Add(watchCh)

	if ptb == nil {
		// Return the tables index so caller can watch it for changes if the trust bundle doesn't exist
		return maxIndexWatchTxn(tx, ws, partitionedIndexEntryName(tablePeeringTrustBundles, q.PartitionOrDefault())), nil, nil
	}
	return ptb.ModifyIndex, ptb, nil
}

// PeeringTrustBundleWrite writes ptb to the state store. If there is an existing trust bundle with the given peer name,
// it will be overwritten.
func (s *Store) PeeringTrustBundleWrite(idx uint64, ptb *pbpeering.PeeringTrustBundle) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	q := Query{
		Value:          ptb.PeerName,
		EnterpriseMeta: *structs.NodeEnterpriseMetaInPartition(ptb.Partition),
	}
	existingRaw, err := tx.First(tablePeeringTrustBundles, indexID, q)
	if err != nil {
		return fmt.Errorf("failed peering trust bundle lookup: %w", err)
	}

	existing, ok := existingRaw.(*pbpeering.PeeringTrustBundle)
	if existingRaw != nil && !ok {
		return fmt.Errorf("invalid type %T", existingRaw)
	}

	if existing != nil {
		ptb.CreateIndex = existing.CreateIndex

	} else {
		ptb.CreateIndex = idx
	}

	ptb.ModifyIndex = idx

	if err := tx.Insert(tablePeeringTrustBundles, ptb); err != nil {
		return fmt.Errorf("failed inserting peering trust bundle: %w", err)
	}

	if err := updatePeeringTrustBundlesTableIndexes(tx, idx, ptb.PartitionOrDefault()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PeeringTrustBundleDelete(idx uint64, q Query) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	existing, err := tx.First(tablePeeringTrustBundles, indexID, q)
	if err != nil {
		return fmt.Errorf("failed peering trust bundle lookup: %v", err)
	}

	if existing == nil {
		return nil
	}

	if err := tx.Delete(tablePeeringTrustBundles, existing); err != nil {
		return fmt.Errorf("failed deleting peering trust bundle: %v", err)
	}

	if err := updatePeeringTrustBundlesTableIndexes(tx, idx, q.PartitionOrDefault()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Snapshot) Peerings() (memdb.ResultIterator, error) {
	return s.tx.Get(tablePeering, indexName)
}

func (s *Snapshot) PeeringTrustBundles() (memdb.ResultIterator, error) {
	return s.tx.Get(tablePeeringTrustBundles, indexID)
}

func (r *Restore) Peering(p *pbpeering.Peering) error {
	if err := r.tx.Insert(tablePeering, p); err != nil {
		return fmt.Errorf("failed restoring peering: %w", err)
	}

	if err := updatePeeringTableIndexes(r.tx, p.ModifyIndex, p.PartitionOrDefault()); err != nil {
		return err
	}

	return nil
}

func (r *Restore) PeeringTrustBundle(ptb *pbpeering.PeeringTrustBundle) error {
	if err := r.tx.Insert(tablePeeringTrustBundles, ptb); err != nil {
		return fmt.Errorf("failed restoring peering trust bundle: %w", err)
	}
	if err := updatePeeringTrustBundlesTableIndexes(r.tx, ptb.ModifyIndex, ptb.PartitionOrDefault()); err != nil {
		return err
	}
	return nil
}

// peersForServiceTxn returns the names of all peers that a service is exported to.
func peersForServiceTxn(
	tx ReadTxn,
	ws memdb.WatchSet,
	serviceName string,
	entMeta *acl.EnterpriseMeta,
) (uint64, []string, error) {
	// Exported service config entries are scoped to partitions so they are in the default namespace.
	partitionMeta := structs.DefaultEnterpriseMetaInPartition(entMeta.PartitionOrDefault())

	idx, rawEntry, err := configEntryTxn(tx, ws, structs.ExportedServices, partitionMeta.PartitionOrDefault(), partitionMeta)
	if err != nil {
		return 0, nil, err
	}
	if rawEntry == nil {
		return idx, nil, err
	}

	entry, ok := rawEntry.(*structs.ExportedServicesConfigEntry)
	if !ok {
		return 0, nil, fmt.Errorf("unexpected type %T for pbpeering.Peering index", rawEntry)
	}

	var (
		wildcardNamespaceIdx = -1
		wildcardServiceIdx   = -1
		exactMatchIdx        = -1
	)

	// Ensure the metadata is defaulted since we make assertions against potentially empty values below.
	// In OSS this is a no-op.
	if entMeta == nil {
		entMeta = acl.DefaultEnterpriseMeta()
	}
	entMeta.Normalize()

	// Services can be exported via wildcards or by their exact name:
	// 		Namespace: *,     Service: *
	// 		Namespace: Exact, Service: *
	// 		Namespace: Exact, Service: Exact
	for i, service := range entry.Services {
		switch {
		case service.Namespace == structs.WildcardSpecifier:
			wildcardNamespaceIdx = i

		case service.Name == structs.WildcardSpecifier && acl.EqualNamespaces(service.Namespace, entMeta.NamespaceOrDefault()):
			wildcardServiceIdx = i

		case service.Name == serviceName && acl.EqualNamespaces(service.Namespace, entMeta.NamespaceOrDefault()):
			exactMatchIdx = i
		}
	}

	var results []string

	// Prefer the exact match over the wildcard match. This matches how we handle intention precedence.
	var targetIdx int
	switch {
	case exactMatchIdx >= 0:
		targetIdx = exactMatchIdx

	case wildcardServiceIdx >= 0:
		targetIdx = wildcardServiceIdx

	case wildcardNamespaceIdx >= 0:
		targetIdx = wildcardNamespaceIdx

	default:
		return idx, results, nil
	}

	for _, c := range entry.Services[targetIdx].Consumers {
		if c.PeerName != "" {
			results = append(results, c.PeerName)
		}
	}
	return idx, results, nil
}

func (s *Store) PeeringListDeleted(ws memdb.WatchSet) (uint64, []*pbpeering.Peering, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	return peeringListDeletedTxn(tx, ws)
}

func peeringListDeletedTxn(tx ReadTxn, ws memdb.WatchSet) (uint64, []*pbpeering.Peering, error) {
	iter, err := tx.Get(tablePeering, indexDeleted, BoolQuery{Value: true})
	if err != nil {
		return 0, nil, fmt.Errorf("failed peering lookup: %v", err)
	}

	// Instead of watching iter.WatchCh() we only need to watch the index entry for the peering table
	// This is sufficient to pick up any changes to peerings.
	idx := maxIndexWatchTxn(tx, ws, tablePeering)

	var result []*pbpeering.Peering
	for t := iter.Next(); t != nil; t = iter.Next() {
		result = append(result, t.(*pbpeering.Peering))
	}

	return idx, result, nil
}
