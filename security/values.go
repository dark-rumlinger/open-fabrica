package security

import "fmt"

// ProcessSecureValue hashes or encrypts a value based on the column's security kind.
// kind "hash" produces a bcrypt hash; kind "encrypt" uses AES encryption. Any other
// kind returns the value unchanged, so callers pass every column through. A nil
// value stays nil (NULL) for every kind. This is the one implementation: every
// write path into a project table uses it.
func ProcessSecureValue(kind string, value any, enc *Encryptor) (any, error) {
	if value == nil {
		return nil, nil
	}
	strVal := fmt.Sprintf("%v", value)
	switch kind {
	case "hash":
		hashed, err := HashPassword(strVal)
		if err != nil {
			return nil, fmt.Errorf("hashing password: %w", err)
		}
		return hashed, nil
	case "encrypt":
		if enc == nil {
			return nil, fmt.Errorf("encryption not configured")
		}
		encrypted, err := enc.Encrypt(strVal)
		if err != nil {
			return nil, fmt.Errorf("encrypting value: %w", err)
		}
		return encrypted, nil
	default:
		return value, nil
	}
}
