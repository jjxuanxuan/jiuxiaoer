// Package catalog 负责酒票套餐定义、公开投影、发布持久化及结算资格事实。
//
// 本包不得导入父级 wineticket 包。父包只通过装配和窄接口连接本子域，
// 外部路由与结算处理器不得依赖本包的具体实现。
package catalog
