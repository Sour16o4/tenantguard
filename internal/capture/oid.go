package capture

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"time"
)

// PostgreSQL type OIDs this package can decode from the binary format.
//
// The set is deliberately small and explicit. A type absent from it is reported
// undecodable rather than guessed at — a wrong value is worse than a known gap,
// because the differ would rebind it and judge the result.
const (
	OIDBool        uint32 = 16
	OIDBytea       uint32 = 17
	OIDName        uint32 = 19
	OIDInt8        uint32 = 20
	OIDInt2        uint32 = 21
	OIDInt4        uint32 = 23
	OIDText        uint32 = 25
	OIDOID         uint32 = 26
	OIDFloat4      uint32 = 700
	OIDFloat8      uint32 = 701
	OIDBPChar      uint32 = 1042
	OIDVarchar     uint32 = 1043
	OIDDate        uint32 = 1082
	OIDTimestamp   uint32 = 1114
	OIDTimestampTZ uint32 = 1184
	OIDUUID        uint32 = 2950
)

// postgresEpoch is the origin PostgreSQL uses for binary temporal types.
var postgresEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// decodeBinary renders a binary-format parameter as text, given its type OID.
//
// ok is false when the OID is unsupported or the payload has the wrong width for
// its type. Callers must treat !ok as "value unknown", never as an empty value.
func decodeBinary(oid uint32, b []byte) (string, bool) {
	switch oid {
	case OIDBool:
		if len(b) != 1 {
			return "", false
		}
		return strconv.FormatBool(b[0] != 0), true

	case OIDInt2:
		if len(b) != 2 {
			return "", false
		}
		return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(b))), 10), true

	case OIDInt4, OIDOID:
		if len(b) != 4 {
			return "", false
		}
		v := int64(int32(binary.BigEndian.Uint32(b)))
		if oid == OIDOID {
			return strconv.FormatUint(uint64(binary.BigEndian.Uint32(b)), 10), true
		}
		return strconv.FormatInt(v, 10), true

	case OIDInt8:
		if len(b) != 8 {
			return "", false
		}
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(b)), 10), true

	case OIDFloat4:
		if len(b) != 4 {
			return "", false
		}
		return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(b))), 'g', -1, 32), true

	case OIDFloat8:
		if len(b) != 8 {
			return "", false
		}
		return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(b)), 'g', -1, 64), true

	case OIDText, OIDVarchar, OIDBPChar, OIDName:
		// These are the same bytes in either format.
		return string(b), true

	case OIDUUID:
		if len(b) != 16 {
			return "", false
		}
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), true

	case OIDDate:
		if len(b) != 4 {
			return "", false
		}
		days := int32(binary.BigEndian.Uint32(b))
		return postgresEpoch.AddDate(0, 0, int(days)).Format("2006-01-02"), true

	case OIDTimestamp, OIDTimestampTZ:
		if len(b) != 8 {
			return "", false
		}
		micros := int64(binary.BigEndian.Uint64(b))
		return postgresEpoch.Add(time.Duration(micros) * time.Microsecond).
			Format("2006-01-02T15:04:05.999999Z07:00"), true

	case OIDBytea:
		return "\\x" + fmt.Sprintf("%x", b), true
	}
	return "", false
}
