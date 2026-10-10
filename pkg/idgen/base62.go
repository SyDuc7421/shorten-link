package idgen

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ToBase62 encodes a positive int64 to a Base62 string.
// Snowflake IDs (2024 epoch) produce 10-character output, 11 from about 2030.
func ToBase62(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 11)
	for n > 0 {
		buf = append(buf, alphabet[n%62])
		n /= 62
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
