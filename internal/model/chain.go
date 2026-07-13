package model

import (
	"encoding/json"
	"time"
)

// ChainConfig 链配置（可动态管理，覆盖/扩展 config.yaml）
type ChainConfig struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	Chain           string    `gorm:"type:varchar(32);uniqueIndex;not null" json:"chain"` // 唯一标识，如 trc20、custom_linea
	Name            string    `gorm:"type:varchar(64);not null" json:"name"`              // 展示名
	ChainType       string    `gorm:"type:varchar(16);not null;default:'evm'" json:"chain_type"` // trx | trc20 | evm | passive
	Enabled         bool      `gorm:"default:false" json:"enabled"`
	RPC             string    `gorm:"type:varchar(500)" json:"rpc"`
	RPCBackups      string    `gorm:"type:text" json:"rpc_backups"` // JSON 字符串数组
	ContractAddress string    `gorm:"type:varchar(100)" json:"contract_address"`
	Confirmations   int       `gorm:"default:12" json:"confirmations"`
	ScanInterval    int       `gorm:"default:30" json:"scan_interval"`
	MaxBlockRange   int       `gorm:"default:0" json:"max_block_range"`
	MaxBatchSize    int       `gorm:"default:0" json:"max_batch_size"`
	BatchDelayMs    int       `gorm:"default:0" json:"batch_delay_ms"`
	RateLimit       float64   `gorm:"type:decimal(10,2);default:0" json:"rate_limit"`
	IsBuiltin       bool      `gorm:"default:false" json:"is_builtin"` // 内置链不可删除
	SortOrder       int       `gorm:"default:0" json:"sort_order"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (ChainConfig) TableName() string {
	return "chain_configs"
}

// GetRPCBackupList 解析备用 RPC 列表
func (c *ChainConfig) GetRPCBackupList() []string {
	if c.RPCBackups == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(c.RPCBackups), &list); err != nil {
		return nil
	}
	return list
}

// SetRPCBackupList 序列化备用 RPC 列表
func (c *ChainConfig) SetRPCBackupList(list []string) {
	if len(list) == 0 {
		c.RPCBackups = "[]"
		return
	}
	b, err := json.Marshal(list)
	if err != nil {
		c.RPCBackups = "[]"
		return
	}
	c.RPCBackups = string(b)
}
