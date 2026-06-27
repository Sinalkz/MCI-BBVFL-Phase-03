package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hyperledger/fabric-chaincode-go/v2/pkg/cid"
	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/ledger/queryresult"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These hand-written fakes back the chaincode with an in-memory ledger. Each
// fake embeds the upstream interface so it satisfies the full method set; we
// override only the methods the chaincode actually calls. Any un-overridden
// method would panic if called (nil embedded interface) — by design, it flags
// accidental reliance on unmodelled shim behaviour.

// compositeKeyNS mirrors the zero-byte namespace/separator Fabric's shim uses so
// our key builder reproduces range-query ordering.
const compositeKeyNS = "\x00"

func createCompositeKey(objectType string, attrs []string) (string, error) {
	ck := compositeKeyNS + objectType + string(rune(0))
	for _, a := range attrs {
		ck += a + string(rune(0))
	}
	return ck, nil
}

func partialCompositeKey(objectType string, attrs []string) string {
	ck := compositeKeyNS + objectType + string(rune(0))
	for _, a := range attrs {
		ck += a + string(rune(0))
	}
	return ck
}

// fakeClientIdentity implements cid.ClientIdentity.
type fakeClientIdentity struct {
	cid.ClientIdentity
	mspID string
}

func (f *fakeClientIdentity) GetMSPID() (string, error) { return f.mspID, nil }

// fakeTxContext implements contractapi.TransactionContextInterface.
type fakeTxContext struct {
	contractapi.TransactionContextInterface
	stub *fakeStub
	ci   *fakeClientIdentity
}

func (c *fakeTxContext) GetStub() shim.ChaincodeStubInterface  { return c.stub }
func (c *fakeTxContext) GetClientIdentity() cid.ClientIdentity { return c.ci }

// fakeStub implements shim.ChaincodeStubInterface over in-memory maps.
type fakeStub struct {
	shim.ChaincodeStubInterface

	world     map[string][]byte            // public world state
	priv      map[string]map[string][]byte // collection -> key -> value
	transient map[string][]byte
	ts        *timestamppb.Timestamp

	nonMember        bool // when true, GetPrivateData rejects (non-member peer)
	lastEventName    string
	lastEventPayload []byte
}

func (s *fakeStub) CreateCompositeKey(objectType string, attrs []string) (string, error) {
	return createCompositeKey(objectType, attrs)
}

func (s *fakeStub) GetState(key string) ([]byte, error) { return s.world[key], nil }

func (s *fakeStub) PutState(key string, value []byte) error {
	s.world[key] = value
	return nil
}

func (s *fakeStub) DelState(key string) error {
	delete(s.world, key)
	return nil
}

func (s *fakeStub) GetTransient() (map[string][]byte, error) { return s.transient, nil }

func (s *fakeStub) PutPrivateData(collection, key string, value []byte) error {
	if s.priv[collection] == nil {
		s.priv[collection] = map[string][]byte{}
	}
	s.priv[collection][key] = value
	return nil
}

func (s *fakeStub) GetPrivateData(collection, key string) ([]byte, error) {
	if s.nonMember {
		return nil, fmt.Errorf("tx creator does not have read access permission on privatedata in chaincode collection %s", collection)
	}
	if s.priv[collection] == nil {
		return nil, nil
	}
	return s.priv[collection][key], nil
}

func (s *fakeStub) SetEvent(name string, payload []byte) error {
	s.lastEventName = name
	s.lastEventPayload = payload
	return nil
}

func (s *fakeStub) GetTxTimestamp() (*timestamppb.Timestamp, error) { return s.ts, nil }

func (s *fakeStub) GetStateByPartialCompositeKey(objectType string, attrs []string) (shim.StateQueryIteratorInterface, error) {
	prefix := partialCompositeKey(objectType, attrs)
	var keys []string
	for k := range s.world {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // zero-padded epoch keys sort in epoch order

	var kvs []*queryresult.KV
	for _, k := range keys {
		kvs = append(kvs, &queryresult.KV{Key: k, Value: s.world[k]})
	}
	return &fakeIterator{kvs: kvs}, nil
}

// fakeIterator implements shim.StateQueryIteratorInterface.
type fakeIterator struct {
	shim.StateQueryIteratorInterface
	kvs []*queryresult.KV
	idx int
}

func (it *fakeIterator) HasNext() bool { return it.idx < len(it.kvs) }

func (it *fakeIterator) Next() (*queryresult.KV, error) {
	kv := it.kvs[it.idx]
	it.idx++
	return kv, nil
}

func (it *fakeIterator) Close() error { return nil }
