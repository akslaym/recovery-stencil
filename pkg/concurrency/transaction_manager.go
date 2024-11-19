package concurrency

import (
	"errors"
	"sync"

	"dinodb/pkg/database"

	"github.com/google/uuid"
)

// Transaction Manager manages all of the transactions on a server.
// Every client runs 1 transaction at a time, so uuid (clientID) can be used to uniquely identify a Transaction.
// Resources are like Entries that can be uniquely identified across tables
type TransactionManager struct {
	resourceLockManager *ResourceLockManager       // Maps every resource to it's corresponding mutex
	waitsForGraph       *WaitsForGraph             // Identifies deadlocks through cycle detection
	transactions        map[uuid.UUID]*Transaction // Identifies the Transaction for a particular client
	mtx                 sync.RWMutex
}

func NewTransactionManager(lm *ResourceLockManager) *TransactionManager {
	return &TransactionManager{
		resourceLockManager: lm,
		waitsForGraph:       NewGraph(),
		transactions:        make(map[uuid.UUID]*Transaction),
	}
}

func (tm *TransactionManager) GetResourceLockManager() (lm *ResourceLockManager) {
	return tm.resourceLockManager
}

func (tm *TransactionManager) GetTransactions() (txs map[uuid.UUID]*Transaction) {
	return tm.transactions
}

// Get a particular transaction of a client.
func (tm *TransactionManager) GetTransaction(clientId uuid.UUID) (tx *Transaction, found bool) {
	tm.mtx.RLock()
	defer tm.mtx.RUnlock()
	tx, found = tm.transactions[clientId]
	return tx, found
}

// Begin a transaction for the given client; error if already began.
func (tm *TransactionManager) Begin(clientId uuid.UUID) error {
	tm.mtx.Lock()
	defer tm.mtx.Unlock()
	_, found := tm.transactions[clientId]
	if found {
		return errors.New("transaction already began")
	}
	tm.transactions[clientId] = &Transaction{clientId: clientId, lockedResources: make(map[Resource]LockType)}
	return nil
}

// Locks the requested resource. Will return an error if deadlock is created by locking.
// 1) Get the transaction we want, and construct the resource.
// 2) Check if we already have rights to the resource
//   - Error if upgrading from read to write locks within this transaction.
//   - Ignore requests for a duplicate lock
//
// 4) Check for deadlocks using waitsForGraph
// 5) Lock resource's mutex
// 6) Add resource to the transaction's resources
// Hint: conflictingTransactions(), GetTransaction()
func (tm *TransactionManager) Lock(clientId uuid.UUID, table database.Index, resourceKey int64, lType LockType) error {
	tm.mtx.RLock()
	tx, found := tm.GetTransaction(clientId)
	res := Resource{table.GetName(), resourceKey}
	if(!found) {
		tm.mtx.RUnlock()
		return errors.New("Could not find transaction with client ID")
	}
	tm.mtx.RUnlock()
	tx.mtx.Lock()
	if existingLockType, hasLock := tx.lockedResources[res]; hasLock {
		if existingLockType == lType || (existingLockType == W_LOCK && lType == R_LOCK) {
			tx.mtx.Unlock()
			return nil
		} else if existingLockType == R_LOCK && lType == W_LOCK {
			tx.mtx.Unlock()
			return errors.New("Trying to upgrade lock type")
		}
	}
	tx.mtx.Unlock()
	for _, conflict := range tm.conflictingTransactions(res, lType) {
		tm.waitsForGraph.AddEdge(tx, conflict)
	}
	if(tm.waitsForGraph.DetectCycle()) {
		return errors.New("We have a cycle")
	}
	err := tm.resourceLockManager.Lock(res, lType)
    if err != nil {
        tx.mtx.Unlock()
        return err
    }

	tx.mtx.Lock()
    tx.lockedResources[res] = lType

    for _, edge := range tm.waitsForGraph.edges {
		if(edge.from == tx) {
			tm.waitsForGraph.RemoveEdge(tx, edge.to)
		}
	}
	tx.mtx.Unlock()
	return nil
}

// Unlocks the requested resource.
// 1) Get the transaction we want, and construct the resource.
// 2) Remove resource from the transaction's currently locked resources if it is valid.
// 3) Unlock resource's mutex
func (tm *TransactionManager) Unlock(clientId uuid.UUID, table database.Index, resourceKey int64, lType LockType) error {
	tm.mtx.RLock()
	tx, found := tm.GetTransaction(clientId)
	res := Resource{table.GetName(), resourceKey}
	if(!found) {
		tm.mtx.RUnlock()
		return errors.New("Could not find transaction with client ID")
	}
	tm.mtx.RUnlock()
	tx.mtx.Lock()

	if existingLockType, hasLock := tx.lockedResources[res]; hasLock {
        if existingLockType != lType {
            tx.mtx.Unlock()
            return errors.New("Locks not of same type")
        }
        delete(tx.lockedResources, res)
    } else {
        tx.mtx.Unlock()
        return errors.New("Resource not in locked resources")
    }

	err := tm.resourceLockManager.Unlock(res, lType)
    if err != nil {
        tx.mtx.Unlock()
        return err
    }
	tx.mtx.Unlock()
	return nil
}

// Commits the given transaction and removes it from the running transactions list.
func (tm *TransactionManager) Commit(clientId uuid.UUID) error {
	tm.mtx.Lock()
	defer tm.mtx.Unlock()
	// Get the transaction we want.
	t, found := tm.transactions[clientId]
	if !found {
		return errors.New("no transactions running")
	}
	// Unlock all resources.
	t.RLock()
	defer t.RUnlock()
	for r, lType := range t.lockedResources {
		err := tm.resourceLockManager.Unlock(r, lType)
		if err != nil {
			return err
		}
	}
	// Remove the transaction from our transactions list.
	delete(tm.transactions, clientId)
	return nil
}

// Returns a slice of all transactions that conflict w/ the given resource and locktype.
func (tm *TransactionManager) conflictingTransactions(r Resource, lType LockType) []*Transaction {
	txs := make([]*Transaction, 0)
	for _, t := range tm.transactions {
		t.RLock()
		for storedResource, storedType := range t.lockedResources {
			if storedResource == r && (storedType == W_LOCK || lType == W_LOCK) {
				txs = append(txs, t)
				break
			}
		}
		t.RUnlock()
	}
	return txs
}
