package model

import (
	"fmt"
	"log"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// DBConfig 数据库连接池配置
type DBConfig struct {
	MaxOpenConns    int           // 最大打开连接数
	MaxIdleConns    int           // 最大空闲连接数
	ConnMaxLifetime time.Duration // 连接最大生命周期
	ConnMaxIdleTime time.Duration // 空闲连接最大生命周期
}

// DefaultDBConfig 默认数据库配置
var DefaultDBConfig = DBConfig{
	MaxOpenConns:    100,
	MaxIdleConns:    10,
	ConnMaxLifetime: time.Hour,
	ConnMaxIdleTime: 10 * time.Minute,
}

// InitDB 初始化数据库连接
func InitDB(dsn string) error {
	return InitDBWithConfig(dsn, DefaultDBConfig)
}

// InitDBWithConfig 使用自定义配置初始化数据库连接（PostgreSQL）
func InitDBWithConfig(dsn string, cfg DBConfig) error {
	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Warn),
		DisableForeignKeyConstraintWhenMigrating: false,
	})
	if err != nil {
		return fmt.Errorf("failed to connect database: %w", err)
	}

	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	if err := sqlDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	if err := autoMigrate(); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	if err := initDefaultData(); err != nil {
		return fmt.Errorf("failed to init default data: %w", err)
	}

	log.Printf("PostgreSQL connected (MaxOpen: %d, MaxIdle: %d)", cfg.MaxOpenConns, cfg.MaxIdleConns)
	return nil
}

// GetDBStats 获取数据库连接池状态
func GetDBStats() map[string]interface{} {
	if DB == nil {
		return nil
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return nil
	}
	stats := sqlDB.Stats()
	return map[string]interface{}{
		"max_open_connections": stats.MaxOpenConnections,
		"open_connections":     stats.OpenConnections,
		"in_use":               stats.InUse,
		"idle":                 stats.Idle,
		"wait_count":           stats.WaitCount,
		"wait_duration":        stats.WaitDuration.String(),
		"max_idle_closed":      stats.MaxIdleClosed,
		"max_lifetime_closed":  stats.MaxLifetimeClosed,
	}
}

// CheckDBHealth 检查数据库健康状态
func CheckDBHealth() error {
	if DB == nil {
		return fmt.Errorf("database not initialized")
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}
	return sqlDB.Ping()
}

// GetDB 获取数据库实例
func GetDB() *gorm.DB {
	return DB
}

// autoMigrate 自动迁移表结构
func autoMigrate() error {
	return DB.AutoMigrate(
		&Merchant{},
		&Order{},
		&Wallet{},
		&SystemConfig{},
		&TransactionLog{},
		&Admin{},
		&Withdrawal{},
		&APILog{},
		&IPBlacklist{},
		&WithdrawAddress{},
		&AppVersion{},
		&ExchangeRate{},
		&ExchangeRateHistory{},
		&BlockScanProgress{},
		&ChainConfig{},
	)
}

// initDefaultData 初始化默认数据
func initDefaultData() error {
	// 系统商户 id=0：仅用于 wallets 外键，不出现在商户列表
	var dupSystemIDs []uint
	DB.Model(&Merchant{}).Where("p_id = ? AND id != 0", "SYSTEM").Pluck("id", &dupSystemIDs)
	if len(dupSystemIDs) > 0 {
		if err := DB.Model(&Wallet{}).Where("merchant_id IN ?", dupSystemIDs).Update("merchant_id", 0).Error; err != nil {
			log.Printf("Warning: reassign wallets from duplicate SYSTEM merchants: %v", err)
		}
		if res := DB.Where("p_id = ? AND id != 0", "SYSTEM").Delete(&Merchant{}); res.Error != nil {
			log.Printf("Warning: Failed to clean duplicate SYSTEM merchants: %v", res.Error)
		} else if res.RowsAffected > 0 {
			log.Printf("Cleaned %d duplicate SYSTEM merchant row(s)", res.RowsAffected)
		}
	}

	var systemMerchant Merchant
	if err := DB.Where("id = ?", 0).First(&systemMerchant).Error; err != nil {
		// PostgreSQL: 允许显式插入 id=0，并修正序列
		result := DB.Exec(`
			INSERT INTO merchants (id, p_id, name, "key", password, status, created_at, updated_at)
			VALUES (0, 'SYSTEM', '系统钱包', 'system_key', '', 1, NOW(), NOW())
			ON CONFLICT (id) DO UPDATE SET p_id = EXCLUDED.p_id, name = EXCLUDED.name`)
		if result.Error != nil {
			// 兼容无 ON CONFLICT 目标的情况（无主键冲突时）
			log.Printf("Warning: Failed to create system merchant (try plain insert): %v", result.Error)
			result = DB.Exec(`
				INSERT INTO merchants (id, p_id, name, "key", password, status, created_at, updated_at)
				VALUES (0, 'SYSTEM', '系统钱包', 'system_key', '', 1, NOW(), NOW())`)
			if result.Error != nil {
				log.Printf("Warning: Failed to create system merchant: %v", result.Error)
			}
		} else {
			log.Println("System merchant (id=0) ensured for global wallets")
		}
		// 保证自增序列不低于 max(id)
		_ = DB.Exec(`SELECT setval(pg_get_serial_sequence('merchants', 'id'), GREATEST(1, (SELECT COALESCE(MAX(id), 1) FROM merchants)))`)
	}

	// 默认管理员
	var adminCount int64
	DB.Model(&Admin{}).Count(&adminCount)
	correctHash := "$2a$10$xiL.DqGTWgs4Sxv99TBxOeUMySHTXe5K2LtTgvtUTNc6wdChhRd7G" // admin123
	if adminCount == 0 {
		admin := Admin{
			Username:           "admin",
			Password:           correctHash,
			Status:             1,
			MustChangePassword: true,
		}
		if err := DB.Create(&admin).Error; err != nil {
			return err
		}
		log.Println("Default admin created: admin / admin123 (请立即修改!)")
	} else {
		DB.Model(&Admin{}).Where("password = ? AND must_change_password = ?", correctHash, false).
			Update("must_change_password", true)
	}

	// 默认商户（排除系统内部商户）
	var merchantCount int64
	DB.Model(&Merchant{}).Where("id > 0 AND p_id != ?", "SYSTEM").Count(&merchantCount)
	defaultMerchantPassword := "$2a$10$ZfUDWHWqrRcGn1mFlMklLudfG4rUnmoIwqaGFMm9ZBSg9CYbLRbvC"
	if merchantCount == 0 {
		merchant := Merchant{
			PID:                "1001",
			Name:               "默认商户",
			Key:                "test_key_123456",
			Password:           defaultMerchantPassword,
			Status:             1,
			MustChangePassword: true,
		}
		if err := DB.Create(&merchant).Error; err != nil {
			return err
		}
		log.Println("Default merchant created: PID=1001, Password=merchant123 (请立即修改!)")
	} else {
		DB.Model(&Merchant{}).Where("password = ? AND must_change_password = ?", defaultMerchantPassword, false).
			Update("must_change_password", true)
	}

	defaultConfigs := []SystemConfig{
		{Key: ConfigKeyRateMode, Value: "hybrid", Description: "汇率模式: auto/manual/hybrid"},
		{Key: ConfigKeyManualRate, Value: "7.2", Description: "手动设置的汇率"},
		{Key: ConfigKeyFloatPercent, Value: "0", Description: "汇率浮动百分比"},
		{Key: ConfigKeyOrderExpire, Value: "30", Description: "订单过期时间(分钟)"},
		{Key: ConfigKeyNotifyRetry, Value: "5", Description: "通知重试次数"},
		{Key: ConfigKeySiteName, Value: "K2Pay", Description: "网站名称"},
		{Key: ConfigKeySystemWalletFeeRate, Value: "0.02", Description: "系统收款码手续费率 (如0.02表示2%)"},
		{Key: ConfigKeyPersonalWalletFeeRate, Value: "0.01", Description: "个人收款码手续费率 (如0.01表示1%)"},
		{Key: ConfigKeyRateAutoUpdate, Value: "1", Description: "汇率自动更新: 1启用 0禁用"},
	}

	for _, cfg := range defaultConfigs {
		var count int64
		DB.Model(&SystemConfig{}).Where(`"key" = ?`, cfg.Key).Count(&count)
		if count == 0 {
			DB.Create(&cfg)
		}
	}

	return nil
}
