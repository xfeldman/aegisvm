package lifecycle

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xfeldman/aegisvm/internal/secrets"
)

// CapabilityToken defines what a guest instance is allowed to do.
// Encrypted with AES-256-GCM using the master key — if you can decrypt it,
// it's authentic (no separate signature needed).
type CapabilityToken struct {
	// Identity
	ParentID string `json:"sub"`           // instance ID this token belongs to
	IssuedAt int64  `json:"iat"`           // unix timestamp
	ExpireAt int64  `json:"exp"`           // unix timestamp

	// Spawn capabilities
	Spawn      bool `json:"spawn"`         // can this instance spawn children?
	SpawnDepth int  `json:"spawn_depth"`   // nesting depth (1 = children can't spawn)

	// Resource ceilings for children
	MaxChildren    int      `json:"max_children"`
	AllowedImages  []string `json:"allowed_images,omitempty"`  // glob-capable
	MaxMemoryMB    int      `json:"max_memory_mb"`
	MaxVCPUs       int      `json:"max_vcpus"`
	AllowedSecrets []string `json:"allowed_secrets,omitempty"`
	MaxExposePorts int      `json:"max_expose_ports"`
}

// GenerateToken creates an encrypted capability token string.
func GenerateToken(ss *secrets.Store, parentID string, caps CapabilityToken) (string, error) {
	caps.ParentID = parentID
	caps.IssuedAt = time.Now().Unix()
	if caps.ExpireAt == 0 {
		caps.ExpireAt = time.Now().Add(24 * time.Hour).Unix()
	}

	data, err := json.Marshal(caps)
	if err != nil {
		return "", fmt.Errorf("marshal token: %w", err)
	}

	encrypted, err := ss.Encrypt(data)
	if err != nil {
		return "", fmt.Errorf("encrypt token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(encrypted), nil
}

// ValidateToken decrypts and validates a capability token.
// Returns the token claims or an error if invalid/expired.
func ValidateToken(ss *secrets.Store, tokenStr string) (*CapabilityToken, error) {
	encrypted, err := base64.RawURLEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}

	data, err := ss.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("invalid token (decrypt failed)")
	}

	var token CapabilityToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("invalid token (unmarshal failed)")
	}

	if time.Now().Unix() > token.ExpireAt {
		return nil, fmt.Errorf("token expired")
	}

	return &token, nil
}

// MintChildToken creates a token for a child instance based on the parent's token.
// Child capabilities are the intersection of parent caps — no escalation possible.
func MintChildToken(ss *secrets.Store, childID string, parent *CapabilityToken) (string, error) {
	child := CapabilityToken{
		SpawnDepth:     parent.SpawnDepth - 1,
		Spawn:          parent.SpawnDepth > 1, // depth 1 = children can't spawn
		MaxChildren:    parent.MaxChildren,
		AllowedImages:  parent.AllowedImages,  // inherited, never expanded
		MaxMemoryMB:    parent.MaxMemoryMB,
		MaxVCPUs:       parent.MaxVCPUs,
		AllowedSecrets: parent.AllowedSecrets,  // inherited, never expanded
		MaxExposePorts: parent.MaxExposePorts,
	}

	return GenerateToken(ss, childID, child)
}
