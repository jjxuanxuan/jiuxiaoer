package money

import "fmt"

type Cents int64

// ValidPositive 判断当前金额是否为有效的正数。
func (c Cents) ValidPositive() bool {
	return c > 0
}

// String 返回当前值的字符串表示。
func (c Cents) String() string {
	return fmt.Sprintf("%d", c)
}
