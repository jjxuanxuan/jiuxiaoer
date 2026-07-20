package validator

import (
	"github.com/go-playground/validator/v10"
)

var instance = validator.New()

// Struct 校验结构体字段是否满足约束。
func Struct(value any) error {
	return instance.Struct(value)
}
