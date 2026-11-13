package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
)

type oidcTransaction struct {
	State       string `json:"state"`
	Org         string `json:"org"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
	Expires     int64  `json:"expires"`
	Assurance   string `json:"assurance,omitempty"`
}

func (t oidcTransaction) configHash() [32]byte {
	context := t.Issuer + "\x00" + t.ClientID + "\x00" + t.RedirectURI
	if t.Assurance != "" {
		context += "\x00flare-provider-assurance-v1\x00" + t.Org + "\x00" + t.Assurance
	}
	return sha256.Sum256([]byte(context))
}

// Collection is bounded to this org and login request. The org lock keeps
// pending rows capped atomically; no scheduler or global cleanup is required.
func (s *Server) storeOIDCTransaction(ctx context.Context, proof oidcTransaction) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.pool == nil {
		return errors.New("OIDC transaction store unavailable")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "oidc-login:"+proof.Org); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM oidc_login_transactions WHERE org_id=$1 AND state_hash IN
		(SELECT state_hash FROM oidc_login_transactions WHERE org_id=$1 AND expires_at<=now() ORDER BY expires_at LIMIT 128)`, proof.Org); err != nil {
		return err
	}
	state, config := sha256.Sum256([]byte(proof.State)), proof.configHash()
	result, err := tx.Exec(ctx, `INSERT INTO oidc_login_transactions(state_hash,org_id,config_hash,expires_at)
		SELECT $1,$2,$3,$4 WHERE (SELECT count(*) FROM oidc_login_transactions WHERE org_id=$2)<128`,
		state[:], proof.Org, config[:], time.Unix(proof.Expires, 0))
	if err != nil || result.RowsAffected() != 1 {
		return errors.New("OIDC pending transaction limit or insert refusal")
	}
	return tx.Commit(ctx)
}

func (s *Server) consumeOIDCTransaction(ctx context.Context, proof oidcTransaction) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.pool == nil {
		return false
	}
	state, config := sha256.Sum256([]byte(proof.State)), proof.configHash()
	result, err := s.pool.Exec(ctx, `DELETE FROM oidc_login_transactions
		WHERE state_hash=$1 AND org_id=$2 AND config_hash=$3 AND expires_at>now()`, state[:], proof.Org, config[:])
	return err == nil && result.RowsAffected() == 1
}
