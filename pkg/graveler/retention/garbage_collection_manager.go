package retention

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/treeverse/lakefs/pkg/db"
	"github.com/treeverse/lakefs/pkg/graveler/ref"

	"github.com/google/uuid"
	"github.com/treeverse/lakefs/pkg/block"
	"github.com/treeverse/lakefs/pkg/graveler"
	"google.golang.org/protobuf/proto"
)

const (
	configFileSuffixTemplate  = "/%s/retention/gc/rules/config.json"
	commitsFileSuffixTemplate = "/%s/retention/gc/commits/run_id=%s/commits.csv"
)

type GarbageCollectionManager struct {
	blockAdapter                block.Adapter
	refManager                  graveler.RefManager
	committedBlockStoragePrefix string
	db                          db.Database
}

type RepositoryCommitGetter struct {
	refManager   graveler.RefManager
	repositoryID graveler.RepositoryID
}

func (r *RepositoryCommitGetter) GetCommit(ctx context.Context, commitID graveler.CommitID) (*graveler.Commit, error) {
	return r.refManager.GetCommit(ctx, r.repositoryID, commitID)
}

func NewGarbageCollectionManager(db db.Database, blockAdapter block.Adapter, refManager graveler.RefManager, committedBlockStoragePrefix string) *GarbageCollectionManager {
	return &GarbageCollectionManager{
		blockAdapter:                blockAdapter,
		refManager:                  refManager,
		committedBlockStoragePrefix: committedBlockStoragePrefix,
		db:                          db,
	}
}

func (m *GarbageCollectionManager) GetRules(ctx context.Context, storageNamespace graveler.StorageNamespace) (*graveler.GarbageCollectionRules, error) {
	reader, err := m.blockAdapter.Get(ctx, block.ObjectPointer{
		StorageNamespace: string(storageNamespace),
		Identifier:       fmt.Sprintf(configFileSuffixTemplate, m.committedBlockStoragePrefix),
		IdentifierType:   block.IdentifierTypeRelative,
	}, -1)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = reader.Close()
	}()
	var rules graveler.GarbageCollectionRules
	rulesBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	err = proto.Unmarshal(rulesBytes, &rules)
	if err != nil {
		return nil, err
	}
	return &rules, nil
}

func (m *GarbageCollectionManager) SaveRules(ctx context.Context, storageNamespace graveler.StorageNamespace, rules *graveler.GarbageCollectionRules) error {
	rulesBytes, err := proto.Marshal(rules)
	if err != nil {
		return err
	}
	return m.blockAdapter.Put(ctx, block.ObjectPointer{
		StorageNamespace: string(storageNamespace),
		Identifier:       fmt.Sprintf(configFileSuffixTemplate, m.committedBlockStoragePrefix),
		IdentifierType:   block.IdentifierTypeRelative,
	}, int64(len(rulesBytes)), bytes.NewReader(rulesBytes), block.PutOpts{})
}

func (m *GarbageCollectionManager) GetRunExpiredCommits(ctx context.Context, storageNamespace graveler.StorageNamespace, runID string) ([]graveler.CommitID, error) {
	if runID == "" {
		return nil, nil
	}
	previousRunReader, err := m.blockAdapter.Get(ctx, block.ObjectPointer{
		Identifier:     string(storageNamespace) + fmt.Sprintf(commitsFileSuffixTemplate, m.committedBlockStoragePrefix, runID),
		IdentifierType: block.IdentifierTypeFull,
	}, -1)
	if err != nil {
		return nil, err
	}
	csvReader := csv.NewReader(previousRunReader)
	csvReader.ReuseRecord = true
	var res []graveler.CommitID
	for {
		commitRow, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if commitRow[1] == "true" {
			res = append(res, graveler.CommitID(commitRow[0]))
		}
	}
	return res, nil
}

func (m *GarbageCollectionManager) SaveGarbageCollectionCommits(ctx context.Context, storageNamespace graveler.StorageNamespace, repositoryID graveler.RepositoryID, rules *graveler.GarbageCollectionRules, previouslyExpiredCommits []graveler.CommitID) (string, error) {
	commitGetter := &RepositoryCommitGetter{
		refManager:   m.refManager,
		repositoryID: repositoryID,
	}
	branchIterator := ref.NewBranchIterator(ctx, m.db, repositoryID, 1000, ref.WithOrderBy("commit_id"))
	commitIterator := ref.NewOrderedCommitIterator(ctx, m.db, repositoryID, 1000, ref.WithAdditionalCondition("NOT EXISTS (SELECT * FROM graveler_commits c2 WHERE c2.repository_id=graveler_commits.repository_id AND c2.parents[1]=graveler_commits.id"))
	gcCommits, err := GetGarbageCollectionCommits(ctx, NewGCStartingPointIterator(commitIterator, branchIterator), commitGetter, rules, previouslyExpiredCommits)
	if err != nil {
		return "", fmt.Errorf("find expired commits: %w", err)
	}
	b := &strings.Builder{}
	csvWriter := csv.NewWriter(b)
	for _, commitID := range gcCommits.expired {
		err := csvWriter.Write([]string{string(commitID), "true"})
		if err != nil {
			return "", err
		}
	}
	for _, commitID := range gcCommits.active {
		err := csvWriter.Write([]string{string(commitID), "false"})
		if err != nil {
			return "", err
		}
	}
	csvWriter.Flush()
	err = csvWriter.Error()
	if err != nil {
		return "", err
	}
	commitsStr := b.String()
	runID := uuid.New().String()
	err = m.blockAdapter.Put(ctx, block.ObjectPointer{
		Identifier:     string(storageNamespace) + fmt.Sprintf(commitsFileSuffixTemplate, m.committedBlockStoragePrefix, runID),
		IdentifierType: block.IdentifierTypeFull,
	}, int64(len(commitsStr)), strings.NewReader(commitsStr), block.PutOpts{})
	if err != nil {
		return "", err
	}
	return runID, nil
}
