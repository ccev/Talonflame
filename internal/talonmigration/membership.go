package talonmigration

import (
	"context"
	"database/sql"
)

// Membership is the read-only geometry index shared by the importer and receiver.
type Membership struct {
	index polygonIndex
}

func LoadMembership(ctx context.Context, conn *sql.Conn) (*Membership, error) {
	index, err := loadPolygons(ctx, conn, true)
	if err != nil {
		return nil, err
	}
	return &Membership{index: index}, nil
}

func (m *Membership) Match(spawnpoint uint64) (string, []int64) {
	result := matchSpawnpoint(spawnpoint, m.index.tree)
	return result.status, result.candidates
}
