package migrator

import (
	"context"
	"fmt"
	"os"

	"github.com/pkg/errors"

	"github.com/iotaledger/hive.go/core/events"
	"github.com/iotaledger/hive.go/core/ioutils"
	"github.com/iotaledger/hive.go/core/syncutils"
	"github.com/iotaledger/hornet/v2/pkg/common"
	iotago "github.com/iotaledger/iota.go/v3"
)

const (
	// SensibleMaxEntriesCount defines an amount of entries within receipts which allows a milestone with 8 parents and 2 sigs/pub keys
	// to fly under the next pow requirement step.
	SensibleMaxEntriesCount = 110
)

var (
	// ErrStateFileAlreadyExists is returned when a new state is tried to be initialized but a state file already exists.
	ErrStateFileAlreadyExists = errors.New("migrator state file already exists")
	// ErrInvalidState is returned when the content of the state file is invalid.
	ErrInvalidState = errors.New("invalid migrator state")
)

// ServiceEvents are events happening around a MigratorService.
type ServiceEvents struct {
	// SoftError is triggered when a soft error is encountered.
	SoftError *events.Event
	// MigratedFundsFetched is triggered when new migration funds were fetched from a legacy node.
	MigratedFundsFetched *events.Event
}

// MigratedFundsCaller is an event caller which gets migrated funds passed.
func MigratedFundsCaller(handler interface{}, params ...interface{}) {
	//nolint:forcetypeassert // we will replace that with generic events anyway
	handler.(func([]*iotago.MigratedFundsEntry))(params[0].([]*iotago.MigratedFundsEntry))
}

// Queryer defines the interface used to query the migrated funds.
type Queryer interface {
	QueryMigratedFunds(iotago.MilestoneIndex) ([]*iotago.MigratedFundsEntry, error)
	QueryNextMigratedFunds(iotago.MilestoneIndex) (iotago.MilestoneIndex, []*iotago.MigratedFundsEntry, error)
}

// Service is a service querying and validating batches of migrated funds.
type Service struct {
	Events *ServiceEvents

	queryer Queryer
	state   State

	mutex      syncutils.Mutex
	migrations chan *migrationResult

	stateFilePath     string
	receiptMaxEntries int
}

// State stores the latest state of the MigratorService.
type State struct {
	LatestMigratedAtIndex iotago.MilestoneIndex `json:"latestMigratedAtIndex"`
	LatestIncludedIndex   uint32                `json:"latestIncludedIndex"`
	SendingReceipt        bool                  `json:"sendingReceipt"`
}

type migrationResult struct {
	stopIndex     iotago.MilestoneIndex
	lastBatch     bool
	migratedFunds []*iotago.MigratedFundsEntry
}

// NewService creates a new MigratorService.
func NewService(queryer Queryer, stateFilePath string, receiptMaxEntries int) *Service {
	return &Service{
		Events: &ServiceEvents{
			SoftError:            events.NewEvent(events.ErrorCaller),
			MigratedFundsFetched: events.NewEvent(MigratedFundsCaller),
		},
		queryer:           queryer,
		migrations:        make(chan *migrationResult),
		receiptMaxEntries: receiptMaxEntries,
		stateFilePath:     stateFilePath,
	}
}

// Receipt returns the next receipt of migrated funds.
// Each receipt can only consists of migrations confirmed by one milestone, it will never be larger than MaxMigratedFundsEntryCount.
// Receipt returns nil, if there are currently no new migrations available. Although the actual API calls and
// validations happen in the background, Receipt might block until the next receipt is ready.
// When s is stopped, Receipt will always return nil.
func (s *Service) Receipt() *iotago.ReceiptMilestoneOpt {
	// make the channel receive and the state update atomic, so that the state always matches the result
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// non-blocking receive; return nil if the channel is closed or value available
	var result *migrationResult
	select {
	case result = <-s.migrations:
	default:
	}
	if result == nil {
		return nil
	}
	s.updateState(result)

	return createReceipt(result.stopIndex, result.lastBatch, result.migratedFunds)
}

// PersistState persists the current state to a file.
// PersistState must be called when the receipt returned by the last call of Receipt has been send to the network.
func (s *Service) PersistState(sendingReceipt bool) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.state.SendingReceipt = sendingReceipt

	// create a backup of the existing migrator state file
	if err := os.Rename(s.stateFilePath, fmt.Sprintf("%s_old", s.stateFilePath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to create backup of migrator state file: %w", err)
	}

	return ioutils.WriteJSONToFile(s.stateFilePath, &s.state, 0660)
}

// InitState initializes the state of s.
// If msIndex is not nil, s is bootstrapped using that index as its initial state,
// otherwise the state is loaded from file.
// The optional utxoManager is used to validate the initialized state against the DB.
// InitState must be called before Start.
func (s *Service) InitState(msIndex *iotago.MilestoneIndex) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var state State
	if msIndex == nil {
		// restore state from file
		if err := ioutils.ReadJSONFromFile(s.stateFilePath, &state); err != nil {
			return fmt.Errorf("failed to load state file: %w", err)
		}
	} else {
		// for bootstrapping the state file must not exist
		if _, err := os.Stat(s.stateFilePath); !os.IsNotExist(err) {
			return ErrStateFileAlreadyExists
		}
		state = State{
			LatestMigratedAtIndex: *msIndex,
			LatestIncludedIndex:   0,
		}
	}

	if state.SendingReceipt {
		return fmt.Errorf("%w: 'sending receipt' flag is set which means the node didn't shutdown correctly", ErrInvalidState)
	}

	// validate the state
	if state.LatestMigratedAtIndex == 0 {
		return fmt.Errorf("%w: latest migrated at index must not be zero", ErrInvalidState)
	}

	//TODO: read this from the latest milestone metadata (https://github.com/iotaledger/inx-coordinator/issues/2)
	//nolint:gocritic // false positive
	//if utxoManager != nil {
	//	highestMigratedAtIndex, err := utxoManager.SearchHighestReceiptMigratedAtIndex()
	//	if err != nil {
	//		return fmt.Errorf("unable to determine highest migrated at index: %w", err)
	//	}
	//	// if highestMigratedAtIndex is zero no receipt in the DB, so we cannot do sanity checks
	//	if highestMigratedAtIndex > 0 && highestMigratedAtIndex != state.LatestMigratedAtIndex {
	//		return fmt.Errorf("state receipt does not match highest receipt in database: state: %d, database: %d",
	//			state.LatestMigratedAtIndex, highestMigratedAtIndex)
	//	}
	//}

	s.state = state

	return nil
}

// OnServiceErrorFunc is a function which is called when the service encounters an
// error which prevents it from functioning properly.
// Returning false from the error handler tells the service to terminate.
type OnServiceErrorFunc func(err error) (terminate bool)

// Start stats the MigratorService s, it stops when the given context is done.
func (s *Service) Start(ctx context.Context, onError OnServiceErrorFunc) {
	var startIndex iotago.MilestoneIndex
	for {
		msIndex, migratedFunds, err := s.nextMigrations(startIndex)
		if err != nil {
			if onError != nil && !onError(err) {
				close(s.migrations)

				return
			}

			continue
		}

		s.Events.MigratedFundsFetched.Trigger(migratedFunds)

		// always continue with the next index
		startIndex = msIndex + 1

		for {
			batch := migratedFunds
			lastBatch := true
			if len(batch) > s.receiptMaxEntries {
				batch = batch[:s.receiptMaxEntries]
				lastBatch = false
			}
			select {
			case s.migrations <- &migrationResult{msIndex, lastBatch, batch}:
			case <-ctx.Done():
				close(s.migrations)

				return
			}
			migratedFunds = migratedFunds[len(batch):]
			if len(migratedFunds) == 0 {
				break
			}
		}
	}
}

// stateMigrations queries the next existing migrations after the current state.
// It returns an empty slice, if the state corresponded to the last migration index of that milestone.
// It returns an error if the current state contains an included migration index that is too large.
func (s *Service) stateMigrations() (iotago.MilestoneIndex, []*iotago.MigratedFundsEntry, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	migratedFunds, err := s.queryer.QueryMigratedFunds(s.state.LatestMigratedAtIndex)
	if err != nil {
		return 0, nil, err
	}
	l := uint32(len(migratedFunds))
	if l >= s.state.LatestIncludedIndex {
		return s.state.LatestMigratedAtIndex, migratedFunds[s.state.LatestIncludedIndex:], nil
	}

	return 0, nil, common.CriticalError(fmt.Errorf("%w: state at index %d but only %d migrations", ErrInvalidState, s.state.LatestIncludedIndex, l))
}

// nextMigrations queries the next existing migrations starting from milestone index startIndex.
// If startIndex is 0 the indices from state are used.
func (s *Service) nextMigrations(startIndex iotago.MilestoneIndex) (iotago.MilestoneIndex, []*iotago.MigratedFundsEntry, error) {
	if startIndex == 0 {
		// for bootstrapping query the migrations corresponding to the state
		msIndex, migratedFunds, err := s.stateMigrations()
		if err != nil {
			return 0, nil, fmt.Errorf("failed to query migrations corresponding to initial state: %w", err)
		}
		// return remaining migrations
		if len(migratedFunds) > 0 {
			return msIndex, migratedFunds, nil
		}
		// otherwise query the next available migrations
		startIndex = msIndex + 1
	}

	return s.queryer.QueryNextMigratedFunds(startIndex)
}

func (s *Service) updateState(result *migrationResult) {
	if result.stopIndex < s.state.LatestMigratedAtIndex {
		panic("invalid stop index")
	}
	// the result increases the latest milestone index
	if result.stopIndex != s.state.LatestMigratedAtIndex {
		s.state.LatestMigratedAtIndex = result.stopIndex
		s.state.LatestIncludedIndex = 0
	}
	s.state.LatestIncludedIndex += uint32(len(result.migratedFunds))
}

func createReceipt(migratedAt iotago.MilestoneIndex, final bool, funds []*iotago.MigratedFundsEntry) *iotago.ReceiptMilestoneOpt {
	// never create an empty receipt
	if len(funds) == 0 {
		return nil
	}

	return &iotago.ReceiptMilestoneOpt{
		MigratedAt: migratedAt,
		Final:      final,
		Funds:      funds,
	}
}
