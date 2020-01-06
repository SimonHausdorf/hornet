package tangle

import (
	"time"

	"github.com/gohornet/hornet/packages/model/milestone_index"
	"github.com/gohornet/hornet/packages/model/tangle"
	"github.com/gohornet/hornet/plugins/gossip"
	"github.com/iotaledger/iota.go/trinary"
)

var (
	milestoneSolidifierWorkerCount = 2 // must be two, so a new request can abort another, in case it is an older milestone
	milestoneSolidifierQueueSize   = 100
	milestoneSolidifierWorkerPool  *workerpool.WorkerPool

	signalChanMilestoneStopSolidification     chan struct{}
	signalChanMilestoneStopSolidificationLock syncutils.Mutex

	solidifierMilestoneIndex     milestone_index.MilestoneIndex = 0
	solidifierMilestoneIndexLock syncutils.RWMutex

	solidifierLock syncutils.RWMutex

	maxMissingMilestoneSearchDepth = 120000 // 1000 TPS at 2 min milestone interval
)

// checkSolidity checks if a single transaction is solid
func checkSolidity(cachedTransaction *tangle.CachedTransaction, addToApproversCache bool) (solid bool, newlySolid bool) {
	cachedTransaction.RegisterConsumer()
	defer cachedTransaction.Release()

	if cachedTransaction.GetTransaction().IsSolid() {
		return true, false
	}

	isSolid := true

	approveeHashes := []trinary.Hash{cachedTransaction.GetTransaction().GetTrunk()}
	if cachedTransaction.GetTransaction().GetTrunk() != cachedTransaction.GetTransaction().GetBranch() {
		approveeHashes = append(approveeHashes, cachedTransaction.GetTransaction().GetBranch())
	}

	for _, approveeHash := range approveeHashes {
		if tangle.SolidEntryPointsContain(approveeHash) {
			// Ignore solid entry points (snapshot milestone included)
			continue
		}

		approveeTx, _ := tangle.GetCachedTransaction(approveeHash)
		if !approveeTx.Exists() || !approveeTx.GetTransaction().IsSolid() {
			isSolid = false

			if addToApproversCache {
				// Add this Transaction as approver to the approvee if it is unknown or not solid yet
				approveeApprovers, _ := tangle.GetApprovers(approveeHash)
				approveeApprovers.Add(cachedTransaction.GetTransaction().GetHash())
			}
			approveeTx.Release()
			break
		}
		approveeTx.Release()
	}

	if isSolid {
		// update the solidity flags of this transaction and its approvers
		cachedTransaction.GetTransaction().SetSolid(true)
		//TODO: change events to use the CachedTransaction?
		Events.TransactionSolid.Trigger(cachedTransaction.GetTransaction())
	}

	return isSolid, isSolid
}

func registerApproverOfApprovee(approver trinary.Hash, approveeHash trinary.Hash, approvers map[string]map[string]struct{}) {
	// The approvee is not solid yet, we need to collect its approvers
	approversMap, exists := approvers[approveeHash]
	if !exists {
		approversMap = make(map[string]struct{})
		approvers[approveeHash] = approversMap
	}

	// Add the main tx to the approvers list of this approvee
	approversMap[approver] = struct{}{}
}

// solidQueueCheck traverses a milestone and checks if it is solid
// Missing tx are requested
// Can be aborted with abortSignal
func solidQueueCheck(milestoneIndex milestone_index.MilestoneIndex, milestoneTail *tangle.CachedTransaction, abortSignal chan struct{}) (solid bool, aborted bool) {

	milestoneTail.RegisterConsumer()
	defer milestoneTail.Release()

	ts := time.Now()

	txsChecked := make(map[string]bool) // isSolid
	txsToTraverse := make(map[string]struct{})
	approvers := make(map[string]map[string]struct{})
	entryTxs := make(map[string]struct{})
	txsToRequest := make(map[string]struct{})
	txsToTraverse[milestoneTail.GetTransaction().GetHash()] = struct{}{}

	// Collect all tx to check by traversing the tangle
	// Loop as long as new transactions are added in every loop cycle
	for len(txsToTraverse) != 0 {

		for txHash := range txsToTraverse {
			select {
			case <-abortSignal:
				return false, true
			default:
				// Go on with the check
			}

			delete(txsToTraverse, txHash)
			isEntryTx := true

			tx, _ := tangle.GetCachedTransaction(txHash)
			if !tx.Exists() {
				log.Panicf("solidQueueCheck: Transaction not found: %v", txHash)
			}

			approveeHashes := []trinary.Hash{tx.GetTransaction().GetTrunk()}
			if tx.GetTransaction().GetTrunk() != tx.GetTransaction().GetBranch() {
				approveeHashes = append(approveeHashes, tx.GetTransaction().GetBranch())
			}

			for _, approveeHash := range approveeHashes {
				if tangle.SolidEntryPointsContain(approveeHash) {
					// Ignore solid entry points (snapshot milestone included)
					continue
				}

				// we add each transaction's approvers to the map, whether the approvee
				// exists or not, as we will not start any concrete solidifiction if any approvee is missing
				registerApproverOfApprovee(tx.GetTransaction().GetHash(), approveeHash, approvers)

				if isSolid, checked := txsChecked[approveeHash]; checked {
					// Tx was already checked
					if !isSolid {
						isEntryTx = false
					}
					continue
				}

				approveeTx, _ := tangle.GetCachedTransaction(approveeHash)
				if !approveeTx.Exists() {
					isEntryTx = false
					txsToRequest[approveeHash] = struct{}{}

					// Mark the tx as checked
					txsChecked[approveeHash] = false
					approveeTx.Release()
					continue
				}

				// Mark the tx as checked
				txsChecked[approveeHash] = approveeTx.GetTransaction().IsSolid()

				if !approveeTx.GetTransaction().IsSolid() {
					isEntryTx = false

					// Traverse this approvee
					txsToTraverse[approveeHash] = struct{}{}
				}
				approveeTx.Release()
			}

			if isEntryTx {
				// Trunk and branch are solid, this tx is an entry point to start the solidify walk
				entryTxs[tx.GetTransaction().GetHash()] = struct{}{}
			}
			tx.Release()
		}
	}
	tc := time.Now()

	if len(txsToRequest) > 0 {
		var txHashes []string
		for txHash := range txsToRequest {
			txHashes = append(txHashes, txHash)
		}
		gossip.RequestMulti(txHashes, milestoneIndex)
		log.Warningf("Stopped solidifier due to missing tx -> Requested missing txs (%d)", len(txHashes))
		return false, false
	}

	if len(entryTxs) == 0 {
		log.Panicf("Solidification failed! No solid entry points for subtangle found! (%d)", milestoneIndex)
	}

	// Loop as long as new solid transactions are found in every loop cycle
	newSolidTxFound := true
	loopCnt := 0
	for newSolidTxFound {
		loopCnt++
		newSolidTxFound = false

		for entryTxHash := range entryTxs {
			select {
			case <-abortSignal:
				return false, true
			default:
				// Go on with the check
			}

			entryTx, _ := tangle.GetCachedTransaction(entryTxHash)
			if !entryTx.Exists() {
				log.Panicf("solidQueueCheck: Transaction not found: %v", entryTxHash)
			}

			if solid, newlySolid := checkSolidity(entryTx, false); solid {
				// Add all tx to the map that approve this solid transaction
				for approverTxHash := range approvers[entryTxHash] {
					entryTxs[approverTxHash] = struct{}{}
				}

				if newlySolid && tangle.IsNodeSynced() {
					// Propagate solidity to the future cone (txs attached to the txs of this milestone)
					entryTx.RegisterConsumer()
					gossipSolidifierWorkerPool.Submit(entryTx)
				}

				// Delete the tx from the map since it is solid
				delete(entryTxs, entryTxHash)
				newSolidTxFound = true
			}
			entryTx.Release()
		}
	}

	// Subtangle is solid if all tx were deleted from the map
	queueSolid := len(entryTxs) == 0

	log.Infof("Solidifier finished (%d): passed: %v, tx: %d, collect: %v, total: %v, entryTx: %d", loopCnt, queueSolid, len(txsChecked), tc.Sub(ts), time.Now().Sub(ts), len(entryTxs))
	return queueSolid, false
}

func abortMilestoneSolidification() {
	signalChanMilestoneStopSolidificationLock.Lock()
	if signalChanMilestoneStopSolidification != nil {
		close(signalChanMilestoneStopSolidification)
		signalChanMilestoneStopSolidification = nil
	}
	signalChanMilestoneStopSolidificationLock.Unlock()
}

// solidifyMilestone tries to solidify the next known non-solid milestone and requests missing tx
func solidifyMilestone(msIndexEmptiedQueue milestone_index.MilestoneIndex) {

	/* How milestone solidification works:

	- A Milestone comes in and gets validated
	- Request milestone trunk/branch without traversion
	- Everytime a request queue gets empty, start the solidifier for the next known non-solid milestone
	- If tx are missing, they are requested by the solidifier
	- If an older queue gets empty than the current solidification index, the traversion can be aborted with a signal and restarted
	- If we miss more than WARP_SYNC_THRESHOLD milestones in our requests, request them via warp sync

	- Special situation startup:
		- RequestMilestonesAndTraverse:
			- loop over all other non-solid milestones from latest to oldest, traverse and request, remove other milestones hashes during the walk
			- this will request all unknown tx in parallel => improve sync speed
			- this should be done without blowing up the RAM
			- don't stop that traversion if older milestone comes in, its only once and helps at startup
	*/

	solidifierMilestoneIndexLock.RLock()
	if (solidifierMilestoneIndex != 0) && (msIndexEmptiedQueue != 0) && (solidifierMilestoneIndex < msIndexEmptiedQueue) {
		// Another older milestone solidification is already running
		solidifierMilestoneIndexLock.RUnlock()
		return
	}
	solidifierMilestoneIndexLock.RUnlock()

	// Stop possible other newer solidifications
	abortMilestoneSolidification()

	solidifierLock.Lock()
	defer solidifierLock.Unlock()

	currentSolidIndex := tangle.GetSolidMilestoneIndex()
	latestIndex := tangle.GetLatestMilestoneIndex()

	if currentSolidIndex == latestIndex && latestIndex != 0 {
		// Latest milestone already solid
		return
	}

	// Always traverse the oldest non-solid milestone, either it gets solid, or something is missing that should be requested.
	milestoneToSolidify := tangle.FindClosestNextMilestone(currentSolidIndex)
	if milestoneToSolidify == nil {
		// No newer milestone available
		return
	}
	milestoneIndexToSolidify := milestoneToSolidify.GetMilestoneIndex()
	setSolidifierMilestoneIndex(milestoneIndexToSolidify)

	signalChanMilestoneStopSolidificationLock.Lock()
	signalChanMilestoneStopSolidification = make(chan struct{})
	signalChanMilestoneStopSolidificationLock.Unlock()

	log.Infof("Run solidity check for Milestone (%d)...", milestoneIndexToSolidify)
	milestoneToSolidifyTail := milestoneToSolidify.GetTail()

	if becameSolid, aborted := solidQueueCheck(milestoneIndexToSolidify, milestoneToSolidifyTail, signalChanMilestoneStopSolidification); !becameSolid {
		if aborted {
			// check was aborted due to older milestones/other solidifier running
			log.Infof("Aborted solid queue check for milestone %d", milestoneIndexToSolidify)
		} else {
			// Milestone not solid yet and missing tx were requested
			log.Infof("Milestone couldn't be solidified! %d", milestoneIndexToSolidify)
		}
		setSolidifierMilestoneIndex(0)
		milestoneToSolidifyTail.Release()
		return
	}

	if (currentSolidIndex + 1) < milestoneIndexToSolidify {

		// Milestone is stable, but some Milestones are missing in between
		// => check if they were found, or search for them in the solidified cone
		closestNextMilestone := tangle.FindClosestNextMilestone(currentSolidIndex)
		if closestNextMilestone == milestoneToSolidify {
			log.Infof("Milestones missing between (%d) and (%d). Search for missing milestones...", currentSolidIndex, milestoneIndexToSolidify)

			// No Milestones found in between => search an older milestone in the solid cone
			if found, aborted := searchMissingMilestone(currentSolidIndex, milestoneIndexToSolidify, milestoneToSolidifyTail, maxMissingMilestoneSearchDepth, signalChanMilestoneStopSolidification); !found {
				if aborted {
					log.Infof("Aborted search for missing milestones between (%d) and (%d).", currentSolidIndex, milestoneIndexToSolidify)
				} else {
					// No milestones found => this should not happen!
					log.Panicf("Milestones missing between (%d) and (%d).", currentSolidIndex, milestoneIndexToSolidify)
				}
			}
		}

		// rerun to solidify the older one
		setSolidifierMilestoneIndex(0)

		milestoneSolidifierWorkerPool.TrySubmit(milestone_index.MilestoneIndex(0))
		return
	}

	tangle.WriteLockLedger()
	defer tangle.WriteUnlockLedger()
	confirmMilestone(milestoneIndexToSolidify, milestoneToSolidifyTail)
	milestoneToSolidifyTail.Release()

	tangle.SetSolidMilestone(milestoneToSolidify)
	tangle.StoreMilestoneInDatabase(milestoneToSolidify)
	Events.SolidMilestoneChanged.Trigger(milestoneToSolidify)
	log.Infof("New solid milestone: %d", milestoneIndexToSolidify)

	// Run check for next milestone
	setSolidifierMilestoneIndex(0)

	milestoneSolidifierWorkerPool.TrySubmit(milestone_index.MilestoneIndex(0))
}

func searchMissingMilestone(solidMilestoneIndex milestone_index.MilestoneIndex, startMilestoneIndex milestone_index.MilestoneIndex, milestoneTail *tangle.CachedTransaction, maxSearchDepth int, abortSignal chan struct{}) (found bool, aborted bool) {

	milestoneTail.RegisterConsumer()
	defer milestoneTail.Release()

	var loopCnt int
	var milestoneFound bool

	ts := time.Now()

	txsChecked := make(map[string]struct{})
	txsToTraverse := make(map[string]struct{})
	txsToTraverse[milestoneTail.GetTransaction().GetHash()] = struct{}{}

	// Search milestones by traversing the tangle
	for loopCnt = 0; (len(txsToTraverse) != 0) && (loopCnt < maxSearchDepth); loopCnt++ {

		for txHash := range txsToTraverse {
			select {
			case <-abortSignal:
				return false, true
			default:
				// Go on with the check
			}
			delete(txsToTraverse, txHash)

			tx, _ := tangle.GetCachedTransaction(txHash)
			if !tx.Exists() {
				log.Panicf("searchMissingMilestone: Transaction not found: %v", txHash)
			}

			approveeHashes := []trinary.Hash{tx.GetTransaction().GetTrunk()}
			if tx.GetTransaction().GetTrunk() != tx.GetTransaction().GetBranch() {
				approveeHashes = append(approveeHashes, tx.GetTransaction().GetBranch())
			}
			tx.Release()

			for _, approveeHash := range approveeHashes {
				if tangle.SolidEntryPointsContain(approveeHash) {
					// Ignore solid entry points (snapshot milestone included)
					continue
				}

				if _, checked := txsChecked[approveeHash]; checked {
					// Tx was already checked
					continue
				}

				approveeTx, _ := tangle.GetCachedTransaction(approveeHash)
				if !approveeTx.Exists() {
					log.Panicf("searchMissingMilestone: Transaction not found: %v", approveeHash)
				}

				if !approveeTx.GetTransaction().IsTail() {
					approveeTx.Release()
					continue
				}

				if tangle.IsMaybeMilestone(approveeTx) {
					// This tx could belong to a milestone
					// => load bundle, and start the milestone check
					bundleBucket, err := tangle.GetBundleBucket(approveeTx.GetTransaction().Tx.Bundle)
					if err != nil {
						log.Panic(err)
					}
					bundle := bundleBucket.GetBundleOfTailTransaction(approveeTx.GetTransaction().Tx.Hash)
					if bundle == nil {
						log.Panicf("searchMissingMilestone: Tx: %v, Bundle not found: %v", approveeHash, approveeTx.GetTransaction().Tx.Bundle)
					}

					isMilestone, err := tangle.CheckIfMilestone(bundle)
					if err != nil {
						log.Infof("searchMissingMilestone: Milestone check failed: %s", err.Error())
					}

					if isMilestone {
						msIndex := bundle.GetMilestoneIndex()
						if (msIndex > solidMilestoneIndex) && (msIndex < startMilestoneIndex) {
							// Milestone found!
							milestoneFound = true
							processValidMilestone(bundle)
							approveeTx.Release()
							break
						}
					}
				}

				approveeTx.Release()

				// Traverse this approvee
				txsToTraverse[approveeHash] = struct{}{}

				// Mark the tx as checked
				txsChecked[approveeHash] = struct{}{}
			}
		}
	}

	log.Infof("searchMissingMilestone finished (%d): found: %v, checked txs: %d, total: %v", loopCnt, milestoneFound, len(txsChecked), time.Now().Sub(ts))
	return milestoneFound, false
}

func setSolidifierMilestoneIndex(index milestone_index.MilestoneIndex) {
	solidifierMilestoneIndexLock.Lock()
	solidifierMilestoneIndex = index
	solidifierMilestoneIndexLock.Unlock()
}

func GetSolidifierMilestoneIndex() milestone_index.MilestoneIndex {
	solidifierMilestoneIndexLock.RLock()
	defer solidifierMilestoneIndexLock.RUnlock()
	return solidifierMilestoneIndex
}
