package capacity

import (
	"errors"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
)

var (
	// ErrLimitInexact reports a quantity that cannot be represented as bytes.
	ErrLimitInexact = errors.New("value cannot be represented as bytes")
	// ErrLimitNotPositive reports a quantity that reserves nothing.
	ErrLimitNotPositive = errors.New("value must be greater than zero")
)

// LimitBytes is the single rule for reading a Pool's reservation limit: the
// declared quantity must parse, be exactly representable in bytes, and be
// positive. Callers wrap the failure in their own diagnosis. An unset limit
// fails as an unparseable quantity; a caller that wants to name that case
// separately checks CapacityLimit before calling.
func LimitBytes(pool volumeapi.Pool) (int64, error) {
	quantity, err := resource.ParseQuantity(pool.CapacityLimit)
	if err != nil {
		return 0, err
	}
	value, exact := quantity.AsInt64()
	if !exact {
		return 0, ErrLimitInexact
	}
	if value <= 0 {
		return 0, ErrLimitNotPositive
	}
	return value, nil
}
