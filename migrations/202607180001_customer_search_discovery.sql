-- +goose Up
CREATE TABLE IF NOT EXISTS customer_search_histories (
  id BIGINT UNSIGNED NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  keyword VARCHAR(128) NOT NULL,
  normalized_keyword VARCHAR(128) NOT NULL,
  search_count INT UNSIGNED NOT NULL DEFAULT 1,
  last_searched_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_customer_search_history_keyword (customer_id, normalized_keyword),
  KEY idx_customer_search_history_recent (customer_id, last_searched_at DESC, id DESC),
  KEY idx_customer_search_history_retention (last_searched_at, id),
  CONSTRAINT chk_customer_search_history_count CHECK (search_count > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS search_keyword_daily_stats (
  id BIGINT UNSIGNED NOT NULL,
  stat_date DATE NOT NULL,
  scope_type VARCHAR(16) NOT NULL,
  scope_id VARCHAR(32) NOT NULL,
  normalized_keyword VARCHAR(128) NOT NULL,
  display_keyword VARCHAR(128) NOT NULL,
  search_count BIGINT UNSIGNED NOT NULL DEFAULT 1,
  last_searched_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_search_keyword_daily_scope (stat_date, scope_type, scope_id, normalized_keyword),
  KEY idx_search_keyword_hot_window (scope_type, scope_id, stat_date, normalized_keyword),
  KEY idx_search_keyword_stats_retention (stat_date, id),
  CONSTRAINT chk_search_keyword_scope_type CHECK (scope_type IN ('global','city')),
  CONSTRAINT chk_search_keyword_daily_count CHECK (search_count > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT IGNORE INTO system_configs
  (id, config_key, config_value, value_type, description, status)
VALUES
  (9601, 'search.hot.default.global', JSON_ARRAY(), 'json', 'C端热门搜索动态榜不足时的全局默认词', 'active'),
  (9602, 'search.hot.blocklist', JSON_ARRAY(), 'json', '禁止进入C端公开热门搜索的归一化关键词', 'active');

-- +goose Down
DELETE FROM system_configs
WHERE (id = 9601 AND config_key = 'search.hot.default.global')
   OR (id = 9602 AND config_key = 'search.hot.blocklist');
DROP TABLE IF EXISTS search_keyword_daily_stats;
DROP TABLE IF EXISTS customer_search_histories;
