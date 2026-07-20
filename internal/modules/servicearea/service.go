package servicearea

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

var cityCodePattern = regexp.MustCompile(`^[0-9]{6}$`)

type Service struct {
	db      *gorm.DB
	repo    *Repository
	redis   *goredis.Client
	config  config.ServiceAreaConfig
	metrics *metrics.Registry
	now     func() time.Time
}

// NewService 创建并初始化服务。
func NewService(cfg config.ServiceAreaConfig, db *gorm.DB, redisClient *goredis.Client, registry *metrics.Registry) *Service {
	return &Service{
		db: db, repo: NewRepository(db), redis: redisClient, config: cfg, metrics: registry,
		now: func() time.Time { return time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)) },
	}
}

// Resolve 返回Resolve。
func (s *Service) Resolve(ctx context.Context, input ResolveInput) (ResolveDTO, error) {
	return s.resolve(ctx, s.db, input, true)
}

// ResolveWithDB 返回Resolve With 数据库。
func (s *Service) ResolveWithDB(ctx context.Context, db *gorm.DB, input ResolveInput) (ResolveDTO, error) {
	return s.resolve(ctx, db, input, false)
}

// Candidates returns at most limit stable straight-line candidates for the
// customer LBS resolver. It performs the same validation and availability
// classification as Resolve but does not use the single-shop cache.
func (s *Service) Candidates(ctx context.Context, input ResolveInput, limit int) ([]ResolvedShop, error) {
	return s.CandidatesWithDB(ctx, s.db, input, limit)
}

func (s *Service) CandidatesWithDB(ctx context.Context, db *gorm.DB, input ResolveInput, limit int) ([]ResolvedShop, error) {
	if err := validateInput(input); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, problem.Internal("database unavailable")
	}
	queryCtx, cancel := context.WithTimeout(ctx, s.config.QueryTimeout)
	defer cancel()
	rows, err := s.repo.Candidates(queryCtx, db, input, s.now(), limit)
	if err != nil {
		if queryCtx.Err() != nil {
			return nil, problem.GatewayTimeout("SERVICE_AREA_TIMEOUT", "service area query timed out")
		}
		return nil, err
	}
	if len(rows) == 0 {
		_, err := s.classifyUnavailable(queryCtx, db, input.CityCode, s.now())
		return nil, err
	}
	return rows, nil
}

// resolve 返回resolve。
func (s *Service) resolve(ctx context.Context, db *gorm.DB, input ResolveInput, allowCache bool) (ResolveDTO, error) {
	if err := validateInput(input); err != nil {
		s.metrics.IncServiceArea("location_required", "bypass")
		return ResolveDTO{}, err
	}
	if db == nil {
		return ResolveDTO{}, problem.Internal("database unavailable")
	}

	cacheKey := ""
	if allowCache && s.redis != nil {
		cacheKey = s.cacheKey(ctx, input)
		var cached ResolveDTO
		if raw, err := s.redis.Get(ctx, cacheKey).Bytes(); err == nil && json.Unmarshal(raw, &cached) == nil {
			s.metrics.IncServiceArea("serviceable", "hit")
			return cached, nil
		}
	}

	queryCtx, cancel := context.WithTimeout(ctx, s.config.QueryTimeout)
	defer cancel()
	now := s.now()
	row, err := s.repo.Resolve(queryCtx, db, input, now)
	if err != nil {
		if !IsNotFound(err) {
			if queryCtx.Err() != nil {
				s.metrics.IncServiceArea("timeout", "miss")
				return ResolveDTO{}, problem.GatewayTimeout("SERVICE_AREA_TIMEOUT", "service area query timed out")
			}
			return ResolveDTO{}, err
		}
		return s.classifyUnavailable(queryCtx, db, input.CityCode, now)
	}

	result := ResolveDTO{ServiceShop: toDTO(row), ResolvedAt: now.UTC()}
	if cacheKey != "" {
		if raw, marshalErr := json.Marshal(result); marshalErr == nil {
			_ = s.redis.Set(ctx, cacheKey, raw, s.config.ResolveCacheTTL).Err()
		}
	}
	s.metrics.IncServiceArea("serviceable", "miss")
	return result, nil
}

// classifyUnavailable 返回classify Unavailable。
func (s *Service) classifyUnavailable(ctx context.Context, db *gorm.DB, cityCode string, now time.Time) (ResolveDTO, error) {
	configured, err := s.repo.HasConfiguredCity(ctx, db, cityCode)
	if err != nil {
		return ResolveDTO{}, err
	}
	if !configured {
		s.metrics.IncServiceArea("city_unsupported", "miss")
		return ResolveDTO{}, problem.New(422, "CITY_UNSUPPORTED", "Unprocessable Entity", "city is not configured for delivery")
	}
	open, err := s.repo.HasOpenShop(ctx, db, cityCode, now)
	if err != nil {
		return ResolveDTO{}, err
	}
	if !open {
		s.metrics.IncServiceArea("no_open_shop", "miss")
		return ResolveDTO{}, problem.Conflict("NO_OPEN_SHOP", "no shop is open for delivery")
	}
	s.metrics.IncServiceArea("out_of_service_area", "miss")
	return ResolveDTO{}, problem.New(422, "OUT_OF_SERVICE_AREA", "Unprocessable Entity", "location is outside the delivery area")
}

// cacheKey 返回缓存密钥。
func (s *Service) cacheKey(ctx context.Context, input ResolveInput) string {
	version := "0"
	if value, err := s.redis.Get(ctx, "service_city_version:"+input.CityCode).Result(); err == nil && value != "" {
		version = value
	}
	return fmt.Sprintf("service_shop:v1:%s:%.5f:%.5f:%s", input.CityCode, input.Latitude, input.Longitude, version)
}

// validateInput 校验输入是否合法。
func validateInput(input ResolveInput) error {
	if !cityCodePattern.MatchString(input.CityCode) || input.Latitude < -90 || input.Latitude > 90 || input.Longitude < -180 || input.Longitude > 180 {
		return problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "valid city_code, lat and lng are required")
	}
	return nil
}

// toDTO 将当前值转换为DTO。
func ToDTO(row ResolvedShop) ShopDTO {
	policy := ""
	if row.OvertimePolicyCode != nil {
		policy = *row.OvertimePolicyCode
	}
	var policyDTO *PolicyDTO
	if row.OvertimePolicyCode != nil && row.OvertimePolicyVersion != nil && row.PolicyTitle != nil && row.PolicySummary != nil {
		policyDTO = &PolicyDTO{Code: *row.OvertimePolicyCode, Version: *row.OvertimePolicyVersion, Title: *row.PolicyTitle, Summary: *row.PolicySummary}
		if row.PolicyTermsURL != nil {
			policyDTO.TermsURL = *row.PolicyTermsURL
		}
	}
	return ShopDTO{
		ID: strconv.FormatUint(row.ID, 10), MerchantID: strconv.FormatUint(row.MerchantID, 10),
		Name: row.Name, CityCode: row.CityCode, District: row.District, Address: row.Address,
		Latitude: row.Latitude, Longitude: row.Longitude, DistanceM: row.DistanceM, Priority: row.Priority, Selectable: true,
		ServiceAreaVersion: row.ServiceAreaVersion,
		DeliveryPromise: DeliveryPromiseDTO{
			DeliveryFeeAmount: row.DeliveryFeeAmount, FreeDeliveryThresholdAmount: row.FreeDeliveryThresholdAmount,
			ETAMinMinutes: row.DeliveryETAMin, ETAMaxMinutes: row.DeliveryETAMax, OvertimePolicyCode: policy, Policy: policyDTO, Confirmed: true,
		},
	}
}

func toDTO(row ResolvedShop) ShopDTO { return ToDTO(row) }
