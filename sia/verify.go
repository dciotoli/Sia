package sia

import (
	"bytes"
	"errors"
	"math/big"
	"sort"
	"time"
)

// Each input has a list of public keys and a required number of signatures.
// This struct keeps track of which public keys have been used and how many
// more signatures are needed.
type InputSignatures struct {
	RemainingSignatures uint8
	PossibleKeys        []PublicKey
	UsedKeys            map[uint8]struct{}
}

// State.validTransaction returns err = nil if the transaction is valid, otherwise
// returns an error explaining what wasn't valid.
func (s *State) validTransaction(t *Transaction) (err error) {
	// Iterate through each input, summing the value, checking for
	// correctness, and creating an InputSignatures object.
	inputSum := Currency(0)
	inputSignaturesMap := make(map[OutputID]InputSignatures)
	for _, input := range t.Inputs {
		// Check the input spends an existing and valid output.
		utxo, exists := s.ConsensusState.UnspentOutputs[input.OutputID]
		if !exists {
			err = errors.New("transaction spends a nonexisting output")
			return
		}

		// Check that the spend conditions match the hash listed in the output.
		if input.SpendConditions.CoinAddress() != s.ConsensusState.UnspentOutputs[input.OutputID].SpendHash {
			err = errors.New("spend conditions do not match hash")
			return
		}

		// Check the timelock on the spend conditions is expired.
		if input.SpendConditions.TimeLock > s.height() {
			err = errors.New("output spent before timelock expiry.")
			return
		}

		// Create the condition for the input signatures and add it to the input signatures map.
		_, exists = inputSignaturesMap[input.OutputID]
		if exists {
			err = errors.New("output spent twice in same transaction")
			return
		}
		var newInputSignatures InputSignatures
		newInputSignatures.RemainingSignatures = input.SpendConditions.NumSignatures
		newInputSignatures.PossibleKeys = input.SpendConditions.PublicKeys
		inputSignaturesMap[input.OutputID] = newInputSignatures

		// Add the input to the coin sum.
		inputSum += utxo.Value
	}

	// Tally up the miner fees and output values.
	outputSum := Currency(0)
	for _, minerFee := range t.MinerFees {
		outputSum += minerFee
	}
	for _, output := range t.Outputs {
		outputSum += output.Value
	}

	// Verify the contracts and tally up the expenditures.
	for _, contract := range t.FileContracts {
		if contract.ContractFund < 0 {
			err = errors.New("contract must be funded.")
			return
		}
		if contract.Start < s.height() {
			err = errors.New("contract must start in the future.")
			return
		}
		if contract.End <= contract.Start {
			err = errors.New("contract duration must be at least one block.")
			return
		}

		outputSum += contract.ContractFund
	}

	for _, proof := range t.StorageProofs {
		// Check that the proof has not already been submitted.
		if s.ConsensusState.OpenContracts[proof.ContractID].WindowSatisfied {
			err = errors.New("storage proof has already been completed for this contract")
			return
		}

		// Check that the proof passes.
	}

	if inputSum != outputSum {
		err = errors.New("inputs do not equal outputs for transaction.")
		return
	}

	for i, sig := range t.Signatures {
		// Check that each signature signs a unique pubkey where
		// RemainingSignatures > 0.
		if inputSignaturesMap[sig.InputID].RemainingSignatures == 0 {
			err = errors.New("friviolous signature detected.")
			return
		}
		_, exists := inputSignaturesMap[sig.InputID].UsedKeys[sig.PublicKeyIndex]
		if exists {
			err = errors.New("public key used twice while signing")
			return
		}

		// Check the timelock on the signature.
		if sig.TimeLock > s.height() {
			err = errors.New("signature timelock has not expired")
			return
		}

		// Check that the signature matches the public key.
		sigHash := t.SigHash(i)
		if !VerifyBytes(sigHash[:], inputSignaturesMap[sig.InputID].PossibleKeys[sig.PublicKeyIndex], sig.Signature) {
			err = errors.New("invalid signature in transaction")
			return
		}
	}

	return
}

// State.addTransactionToPool() adds a transaction to the transaction pool and
// transaction list without verifying it.
func (s *State) addTransactionToPool(t *Transaction) {
	for _, input := range t.Inputs {
		s.ConsensusState.TransactionPool[input.OutputID] = t
	}
	s.ConsensusState.TransactionList[t.Inputs[0].OutputID] = t
}

// Takes a transaction out of the transaction pool & transaction list.
func (s *State) removeTransactionFromPool(t *Transaction) {
	for _, input := range t.Inputs {
		delete(s.ConsensusState.TransactionPool, input.OutputID)
	}
	delete(s.ConsensusState.TransactionList, t.Inputs[0].OutputID)
}

// State.AcceptTransaction() checks for a conflict of the transaction with the
// transaction pool, then checks that the transaction is valid given the
// current state, then adds the transaction to the transaction pool.
// AcceptTransaction() is thread safe, and can be called concurrently.
func (s *State) AcceptTransaction(t Transaction) (err error) {
	s.Lock()
	defer s.Unlock()

	// Check that the transaction is not in conflict with the transaction
	// pool.
	for _, input := range t.Inputs {
		_, exists := s.ConsensusState.TransactionPool[input.OutputID]
		if exists {
			err = errors.New("conflicting transaction exists in transaction pool")
			return
		}
	}

	// Check that the transaction is potentially valid.
	err = s.validTransaction(&t)
	if err != nil {
		return
	}

	// Add the transaction to the pool.
	s.addTransactionToPool(&t)

	return
}

// State.checkMaps() looks through the maps known to the state and sees if the
// block id has been cached anywhere.
func (s *State) checkMaps(b *Block) (parentBlockNode *BlockNode, err error) {
	// See if the block is a known invalid block.
	_, exists := s.BadBlocks[b.ID()]
	if exists {
		err = errors.New("block is known to be invalid")
		return
	}

	// See if the block is a known valid block.
	_, exists = s.BlockMap[b.ID()]
	if exists {
		err = errors.New("Block exists in block map.")
		return
	}

	/*
		// See if the block is a known orphan.
		_, exists = s.OrphanBlocks[b.ID()]
		if exists {
			err = errors.New("Block exists in orphan list")
			return
		}
	*/

	// See if the block's parent is known.
	parentBlockNode, exists = s.BlockMap[b.ParentBlock]
	if !exists {
		// OrphanBlocks[b.ID()] = b
		err = errors.New("Block is an orphan")
		return
	}

	return
}

// Block.expectedTransactionMerkleRoot() returns the expected transaction
// merkle root of the block.
func (b *Block) expectedTransactionMerkleRoot() Hash {
	var transactionHashes []Hash
	for _, transaction := range b.Transactions {
		transactionHashes = append(transactionHashes, HashBytes(Marshal(transaction)))
	}
	return MerkleRoot(transactionHashes)
}

// Block.checkTarget() returns true if the block id is lower than the target.
func (b *Block) checkTarget(target Target) bool {
	blockHash := b.ID()
	return bytes.Compare(target[:], blockHash[:]) >= 0
}

// State.validateHaeader() returns `err = nil` if the header information in the
// block (everything except the transactions) is valid, and returns an error
// explaining why validation failed if the header is invalid.
func (s *State) validateHeader(parent *BlockNode, b *Block) (err error) {
	// Check that the block is not too far in the future.
	skew := b.Timestamp - Timestamp(time.Now().Unix())
	if skew > FutureThreshold {
		// Do something so that you will return to considering this
		// block once it's no longer too far in the future.
		err = errors.New("timestamp too far in future")
		return
	}

	// If timestamp is too far in the past, reject and put in bad blocks.
	var intTimestamps []int
	for _, timestamp := range parent.RecentTimestamps {
		intTimestamps = append(intTimestamps, int(timestamp))
	}
	sort.Ints(intTimestamps)
	if Timestamp(intTimestamps[5]) > b.Timestamp {
		s.BadBlocks[b.ID()] = struct{}{}
		err = errors.New("timestamp invalid for being in the past")
		return
	}

	// Check that the transaction merkle root matches the transactions
	// included into the block.
	if b.MerkleRoot != b.expectedTransactionMerkleRoot() {
		s.BadBlocks[b.ID()] = struct{}{}
		err = errors.New("merkle root does not match transactions sent.")
		return
	}

	// Check the id meets the target.
	if !b.checkTarget(parent.Target) {
		err = errors.New("block does not meet target")
		return
	}

	return
}

// State.childTarget() calculates the proper target of a child node given the
// parent node, and copies the target into the child node.
func (s *State) childTarget(parentNode *BlockNode, newNode *BlockNode) (target Target) {
	var timePassed, expectedTimePassed Timestamp
	blockWindow := TargetWindow
	if newNode.Height < TargetWindow {
		blockWindow = newNode.Height
		timePassed = newNode.Block.Timestamp - s.BlockRoot.Block.Timestamp
		expectedTimePassed = TargetSecondsPerBlock * Timestamp(newNode.Height)
	} else {
		// WARNING: this code assumes that the block at height
		// newNode.Height-TargetWindow is the same for both the new
		// node and the currenct fork. In general, this is a safe
		// assumption, because there should never be a reorg that's
		// 5000 blocks long.
		adjustmentBlock := s.blockAtHeight(newNode.Height - TargetWindow)
		timePassed = newNode.Block.Timestamp - adjustmentBlock.Timestamp
		expectedTimePassed = TargetSecondsPerBlock * Timestamp(TargetWindow)
	}

	// Adjustment = timePassed / expectedTimePassed / blockWindow.
	targetAdjustment := big.NewRat(int64(timePassed), int64(expectedTimePassed)*int64(blockWindow))

	// Enforce a maximum targetAdjustment
	if targetAdjustment.Cmp(MaxAdjustmentUp) == 1 {
		targetAdjustment = MaxAdjustmentUp
	} else if targetAdjustment.Cmp(MaxAdjustmentDown) == -1 {
		targetAdjustment = MaxAdjustmentDown
	}

	// Take the target adjustment and apply it to the target slice,
	// using rational numbers. Truncate the result.
	oldTarget := new(big.Int).SetBytes(parentNode.Target[:])
	ratOldTarget := new(big.Rat).SetInt(oldTarget)
	ratNewTarget := ratOldTarget.Mul(targetAdjustment, ratOldTarget)
	intNewTarget := new(big.Int).Div(ratNewTarget.Num(), ratNewTarget.Denom())
	newTargetBytes := intNewTarget.Bytes()
	offset := len(target[:]) - len(newTargetBytes)
	copy(target[offset:], newTargetBytes)
	return
}

// State.childDepth() returns the cumulative weight of all the blocks leading
// up to and including the child block.
func (s *State) childDepth(parentNode *BlockNode) BlockWeight {
	blockWeight := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).SetBytes(parentNode.Target[:]))
	return BlockWeight(new(big.Rat).Add(parentNode.Depth, blockWeight))
}

// State.addBlockToTree() takes a block and a parent node, and adds a child
// node to the parent containing the block. No validation is done.
func (s *State) addBlockToTree(parentNode *BlockNode, b *Block) (newNode *BlockNode) {
	// Create the child node.
	newNode = new(BlockNode)
	newNode.Block = b
	newNode.Height = parentNode.Height + 1

	// Copy over the timestamps.
	copy(newNode.RecentTimestamps[:], parentNode.RecentTimestamps[1:])
	newNode.RecentTimestamps[10] = b.Timestamp

	// Calculate target and depth.
	newNode.Target = s.childTarget(parentNode, newNode)
	newNode.Depth = s.childDepth(parentNode)

	// Add the node to the block map and the list of its parents children.
	s.BlockMap[b.ID()] = newNode
	parentNode.Children = append(parentNode.Children, newNode)

	return
}

// State.heavierFork() returns ture if the input node is 5% heavier than the
// current node of the ConesnsusState.
func (s *State) heavierFork(newNode *BlockNode) bool {
	threshold := new(big.Rat).Mul(s.currentBlockWeight(), SurpassThreshold)
	requiredDepth := new(big.Rat).Add(s.currentDepth(), threshold)
	return (*big.Rat)(newNode.Depth).Cmp(requiredDepth) == 1
}

// State.reverseTransaction removes a given transaction from the
// ConsensusState, making it as though the transaction had never happened.
func (s *State) reverseTransaction(t Transaction) {
	// Remove all outputs.
	for i := range t.Outputs {
		delete(s.ConsensusState.UnspentOutputs, t.outputID(i))
	}

	// Add all outputs spent by inputs.
	for _, input := range t.Inputs {
		s.ConsensusState.UnspentOutputs[input.OutputID] = s.ConsensusState.SpentOutputs[input.OutputID]
		delete(s.ConsensusState.SpentOutputs, input.OutputID)
	}

	// Delete all outputs created by storage proofs.
	for _, sp := range t.StorageProofs {
		openContract := s.ConsensusState.OpenContracts[sp.ContractID]
		outputID := openContract.storageProofOutputID(s.height(), true)
		delete(s.ConsensusState.UnspentOutputs, outputID)
	}

	// Delete all the open contracts created by new contracts.
	for i := range t.FileContracts {
		contractID := t.fileContractID(i)
		delete(s.ConsensusState.OpenContracts, contractID)
	}
}

// State.rewindABlock() removes the most recent block from the ConsensusState,
// making the ConsensusState as though the block had never been integrated.
func (s *State) rewindABlock() {
	// Repen all contracts that terminated, and remove the corresponding output.
	for _, openContract := range s.currentBlockNode().ContractTerminations {
		s.ConsensusState.OpenContracts[openContract.ContractID] = openContract
		delete(s.ConsensusState.UnspentOutputs, openContract.fileContractTerminationOutputID())
	}

	// Reverse all outputs created by missed storage proofs.
	for _, missedProof := range s.currentBlockNode().MissedStorageProofs {
		s.ConsensusState.OpenContracts[missedProof.ContractID].FundsRemaining += s.ConsensusState.UnspentOutputs[missedProof.OutputID].Value
		s.ConsensusState.OpenContracts[missedProof.ContractID].Failures -= 1
		delete(s.ConsensusState.UnspentOutputs, missedProof.OutputID)
	}

	// Reverse each transaction in the block, in reverse order from how
	// they appear in the block.
	for i := len(s.currentBlock().Transactions) - 1; i >= 0; i-- {
		s.reverseTransaction(s.currentBlock().Transactions[i])
		s.addTransactionToPool(&s.currentBlock().Transactions[i])
	}

	// Update the CurrentBlock and CurrentPath variables of the
	// ConsensusState.
	s.ConsensusState.CurrentBlock = s.currentBlock().ParentBlock
	delete(s.ConsensusState.CurrentPath, s.height())
}

// State.applyTransaction() takes a transaction and adds it to the
// ConsensusState, updating the list of contracts, outputs, etc.
func (s *State) applyTransaction(t Transaction) {
	// Remove all inputs from the unspent outputs list.
	for _, input := range t.Inputs {
		s.ConsensusState.SpentOutputs[input.OutputID] = s.ConsensusState.UnspentOutputs[input.OutputID]
		delete(s.ConsensusState.UnspentOutputs, input.OutputID)
	}

	// Add all outputs to the unspent outputs list
	for i, output := range t.Outputs {
		s.ConsensusState.UnspentOutputs[t.outputID(i)] = output
	}

	// Add all new contracts to the OpenContracts list.
	for i, contract := range t.FileContracts {
		contractID := t.fileContractID(i)
		openContract := OpenContract{
			FileContract:    contract,
			ContractID:      contractID,
			FundsRemaining:  contract.ContractFund,
			Failures:        0,
			WindowSatisfied: true, // The first window is free, because the start is in the future by mandate.
		}
		s.ConsensusState.OpenContracts[contractID] = &openContract
	}

	// Add all outputs created by storage proofs.
	for _, sp := range t.StorageProofs {
		// Check for contract termination.
		openContract := s.ConsensusState.OpenContracts[sp.ContractID]
		payout := openContract.FileContract.ValidProofPayout
		if openContract.FundsRemaining < openContract.FileContract.ValidProofPayout {
			payout = openContract.FundsRemaining
		}

		output := Output{
			Value:     payout,
			SpendHash: openContract.FileContract.ValidProofAddress,
		}
		s.ConsensusState.UnspentOutputs[openContract.storageProofOutputID(s.height(), true)] = output

		// Mark the proof as complete for this window.
		s.ConsensusState.OpenContracts[sp.ContractID].WindowSatisfied = true
		s.ConsensusState.OpenContracts[sp.ContractID].FundsRemaining -= payout
	}
}

// s.integrateBlock() will verify the block and then integrate it into the
// consensus state.
func (s *State) integrateBlock(b *Block) (err error) {
	var appliedTransactions []Transaction
	minerSubsidy := Currency(0)
	for _, txn := range b.Transactions {
		err = s.validTransaction(&txn)
		if err != nil {
			s.BadBlocks[b.ID()] = struct{}{}
			break
		}

		// Apply the transaction to the ConsensusState, adding it to the list of applied transactions.
		s.applyTransaction(txn)
		appliedTransactions = append(appliedTransactions, txn)

		// Remove the inputs from the transaction pool.
		s.removeTransactionFromPool(&txn)

		// Add the miner fees to the miner subsidy.
		for _, fee := range txn.MinerFees {
			minerSubsidy += fee
		}
	}

	if err != nil {
		// Rewind transactions added to ConsensusState.
		for i := len(appliedTransactions) - 1; i >= 0; i-- {
			s.reverseTransaction(appliedTransactions[i])
		}
		return
	}

	// Perform maintanence on all open contracts.
	//
	// This could be split into its own function.
	var contractsToDelete []ContractID
	for _, openContract := range s.ConsensusState.OpenContracts {
		// Check for the window switching over.
		if (s.height()-openContract.FileContract.Start)%openContract.FileContract.ChallengeFrequency == 0 && s.height() > openContract.FileContract.Start {
			// Check for a missed proof.
			if openContract.WindowSatisfied == false {
				payout := openContract.FileContract.MissedProofPayout
				if openContract.FundsRemaining < openContract.FileContract.MissedProofPayout {
					payout = openContract.FundsRemaining
				}

				newOutputID := openContract.storageProofOutputID(s.height(), false)
				output := Output{
					Value:     payout,
					SpendHash: openContract.FileContract.MissedProofAddress,
				}
				s.ConsensusState.UnspentOutputs[newOutputID] = output
				msp := MissedStorageProof{
					OutputID:   newOutputID,
					ContractID: openContract.ContractID,
				}
				s.currentBlockNode().MissedStorageProofs = append(s.currentBlockNode().MissedStorageProofs, msp)

				// Update the FundsRemaining
				openContract.FundsRemaining -= payout

				// Update the failures count.
				openContract.Failures += 1
			}
			openContract.WindowSatisfied = false
		}

		// Check for a terminated contract.
		if openContract.FundsRemaining == 0 || openContract.FileContract.End == s.height() || openContract.FileContract.Tolerance == openContract.Failures {
			if openContract.FundsRemaining != 0 {
				// Create a new output that terminates the contract.
				outputID := openContract.fileContractTerminationOutputID()
				output := Output{
					Value: openContract.FundsRemaining,
				}
				if openContract.FileContract.Tolerance == openContract.Failures {
					output.SpendHash = openContract.FileContract.MissedProofAddress
				} else {
					output.SpendHash = openContract.FileContract.ValidProofAddress
				}
				s.ConsensusState.UnspentOutputs[outputID] = output
			}

			// Add the contract to contract terminations.
			s.currentBlockNode().ContractTerminations = append(s.currentBlockNode().ContractTerminations, openContract)

			// Mark contract for deletion (can't delete from a map while
			// iterating through it - results in undefined behavior of the
			// iterator.
			contractsToDelete = append(contractsToDelete, openContract.ContractID)
		}
	}
	// Delete all of the contracts that terminated.
	for _, contractID := range contractsToDelete {
		delete(s.ConsensusState.OpenContracts, contractID)
	}

	// Add coin inflation to the miner subsidy.
	minerSubsidy += 1000

	// Add output contianing miner fees + block subsidy.
	minerSubsidyOutput := Output{
		Value:     minerSubsidy,
		SpendHash: b.MinerAddress,
	}
	s.ConsensusState.UnspentOutputs[b.subsidyID()] = minerSubsidyOutput

	// Update the current block and current path variables of the ConsensusState.
	s.ConsensusState.CurrentBlock = b.ID()
	s.ConsensusState.CurrentPath[s.BlockMap[b.ID()].Height] = b.ID()

	return
}

// State.invalidateNode() is a recursive function that deletes all of the
// children of a block and puts them on the bad blocks list.
func (s *State) invalidateNode(node *BlockNode) {
	for i := range node.Children {
		s.invalidateNode(node.Children[i])
	}

	delete(s.BlockMap, node.Block.ID())
	s.BadBlocks[node.Block.ID()] = struct{}{}
}

// State.forkBlockchain() will go from the current block over to a block on a
// different fork, rewinding and integrating blocks as needed. forkBlockchain()
// will return an error if any of the blocks in the new fork are invalid.
func (s *State) forkBlockchain(newNode *BlockNode) (err error) {
	// Find the common parent between the new fork and the current
	// fork, keeping track of which path is taken through the
	// children of the parents so that we can re-trace as we
	// validate the blocks.
	currentNode := newNode
	value := s.ConsensusState.CurrentPath[currentNode.Height]
	var parentHistory []BlockID
	for value != currentNode.Block.ID() {
		parentHistory = append(parentHistory, currentNode.Block.ID())
		currentNode = s.BlockMap[currentNode.Block.ParentBlock]
		value = s.ConsensusState.CurrentPath[currentNode.Height]
	}

	// Remove blocks from the ConsensusState until we get to the
	// same parent that we are forking from.
	var rewoundBlocks []BlockID
	for s.ConsensusState.CurrentBlock != currentNode.Block.ID() {
		rewoundBlocks = append(rewoundBlocks, s.ConsensusState.CurrentBlock)
		s.rewindABlock()
	}

	// Validate each block in the parent history in order, updating
	// the state as we go.  If at some point a block doesn't
	// verify, you get to walk all the way backwards and forwards
	// again.
	validatedBlocks := 0
	for i := len(parentHistory) - 1; i >= 0; i-- {
		err = s.integrateBlock(s.BlockMap[parentHistory[i]].Block)
		if err != nil {
			// Add the whole tree of blocks to BadBlocks,
			// deleting them from BlockMap
			s.invalidateNode(s.BlockMap[parentHistory[i]])

			// Rewind the validated blocks
			for i := 0; i < validatedBlocks; i++ {
				s.rewindABlock()
			}

			// Integrate the rewound blocks
			for i := len(rewoundBlocks) - 1; i >= 0; i-- {
				err = s.integrateBlock(s.BlockMap[rewoundBlocks[i]].Block)
				if err != nil {
					panic("Once-validated blocks are no longer validating - state logic has mistakes.")
				}
			}

			break
		}
		validatedBlocks += 1
	}

	return
}

// State.AcceptBlock() is a thread-safe function that will add blocks to the
// state, forking the blockchain if they are on a fork that is heavier than the
// current fork. AcceptBlock() can be called concurrently.
func (s *State) AcceptBlock(b Block) (err error) {
	s.Lock()
	defer s.Unlock()

	// Check the maps in the state to see if the block is already known.
	parentBlockNode, err := s.checkMaps(&b)
	if err != nil {
		return
	}

	// Check that the header of the block is valid.
	err = s.validateHeader(parentBlockNode, &b)
	if err != nil {
		return
	}

	newBlockNode := s.addBlockToTree(parentBlockNode, &b)

	// If the new node is 5% heavier than the current node, switch to the new fork.
	if s.heavierFork(newBlockNode) {
		err = s.forkBlockchain(newBlockNode)
		if err != nil {
			return
		}
	}

	return
}