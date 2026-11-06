package chatgpt

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

type identity struct {
	Subject string
	Email   string
}

// validateIDToken accepts only OpenAI's advertised RS256 signatures and fetches
// keys from its fixed issuer, never from URLs inside untrusted JWT headers.
func (s *Store) validateIDToken(ctx context.Context, token, clientID, nonce string) (identity, error) {
	invalid := errors.New("invalid ChatGPT identity token")
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > maxCredentialBytes || nonce == "" || clientID == "" {
		return identity{}, invalid
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return identity{}, invalid
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return identity{}, invalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity{}, invalid
	}
	var claims struct {
		Issuer          string         `json:"iss"`
		Audience        jsontext.Value `json:"aud"`
		Subject         string         `json:"sub"`
		Email           string         `json:"email"`
		Nonce           string         `json:"nonce"`
		Expires         int64          `json:"exp"`
		Issued          int64          `json:"iat"`
		NotBefore       int64          `json:"nbf"`
		AuthorizedParty string         `json:"azp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer != issuer || claims.Subject == "" || claims.Expires <= time.Now().Unix() || claims.NotBefore > time.Now().Unix() || claims.Issued > time.Now().Add(time.Minute).Unix() || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return identity{}, invalid
	}
	var audiences []string
	var singleAudience string
	if json.Unmarshal(claims.Audience, &singleAudience) == nil {
		audiences = []string{singleAudience}
	} else if json.Unmarshal(claims.Audience, &audiences) != nil {
		return identity{}, invalid
	}
	matched := false
	for _, audience := range audiences {
		if audience == clientID {
			matched = true
		}
	}
	if !matched || (len(audiences) > 1 && claims.AuthorizedParty != clientID) || (claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID) {
		return identity{}, invalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity{}, invalid
	}
	endpoint := s.jwksURL
	if endpoint == "" {
		endpoint = issuer + "/.well-known/jwks.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return identity{}, invalid
	}
	response, err := s.httpClient().Do(req)
	if err != nil {
		return identity{}, errors.New("cannot fetch ChatGPT identity verification keys")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return identity{}, errors.New("cannot fetch ChatGPT identity verification keys")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCredentialBytes+1))
	if err != nil || len(body) > maxCredentialBytes {
		return identity{}, errors.New("cannot read ChatGPT identity verification keys")
	}
	var keys struct {
		Keys []struct {
			Type      string `json:"kty"`
			ID        string `json:"kid"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(body, &keys) != nil {
		return identity{}, errors.New("invalid ChatGPT identity verification keys")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, key := range keys.Keys {
		if key.ID != header.KeyID || key.Type != "RSA" || (key.Use != "" && key.Use != "sig") || (key.Algorithm != "" && key.Algorithm != "RS256") {
			continue
		}
		n, nerr := base64.RawURLEncoding.DecodeString(key.Modulus)
		e, eerr := base64.RawURLEncoding.DecodeString(key.Exponent)
		if nerr != nil || eerr != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		modulus := new(big.Int).SetBytes(n)
		exponent := new(big.Int).SetBytes(e).Int64()
		if modulus.BitLen() < 2048 || modulus.BitLen() > 8192 || exponent < 3 || exponent > 1<<31-1 || exponent%2 == 0 {
			continue
		}
		public := &rsa.PublicKey{N: modulus, E: int(exponent)}
		if rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature) == nil {
			return identity{Subject: claims.Subject, Email: claims.Email}, nil
		}
	}
	return identity{}, invalid
}
