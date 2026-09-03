package controller

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func intstrFromInt(value int) intstr.IntOrString { return intstr.FromInt32(int32(value)) }
func boolPointer(value bool) *bool               { return &value }
func int64Pointer(value int64) *int64            { return &value }
func hostPathTypePointer(value corev1.HostPathType) *corev1.HostPathType {
	return &value
}
func errorsJoin(values ...error) error { return errors.Join(values...) }
