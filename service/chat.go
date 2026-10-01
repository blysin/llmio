package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/atopos31/llmio/balancers"
	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/pkg/token"
	"github.com/atopos31/llmio/providers"
	"github.com/samber/lo"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

func BalanceChat(ctx context.Context, start time.Time, style string, before Before, providersWithMeta ProvidersWithMeta, reqMeta models.ReqMeta) (*http.Response, *models.ChatLog, error) {
	slog.Info("request", "model", before.Model, "stream", before.Stream, "tool_call", before.toolCall, "structured_output", before.structuredOutput, "image", before.image)

	providerMap := providersWithMeta.ProviderMap

	// 收集重试过程中的err日志
	retryLog := make(chan models.ChatLog, providersWithMeta.MaxRetry)
	defer close(retryLog)

	go RecordRetryLog(context.Background(), retryLog)

	// 选择负载均衡策略
	// 主池与备用池各自独立构建：备用关联不参与日常轮询，
	// 只有主池在本请求内被清空（所有主关联都失败出局）后才会接管，且只降不升。
	newBalancer := func(items map[uint]int) balancers.Balancer {
		var b balancers.Balancer
		switch providersWithMeta.Strategy {
		case consts.BalancerRotor:
			b = balancers.NewRotor(items)
		default:
			b = balancers.NewLottery(items)
		}
		// 是否开启熔断
		if providersWithMeta.Breaker {
			b = balancers.BalancerWrapperBreaker(b)
		}
		return b
	}

	// balancer 在本请求内可变：主池耗尽后指向备用池
	balancer := newBalancer(providersWithMeta.WeightItems)
	backupBalancer := newBalancer(providersWithMeta.BackupWeightItems)
	hasBackup := len(providersWithMeta.BackupWeightItems) > 0
	usingBackup := false

	// 降级状态提示：终态错误与候选池中途清空都要带上它，
	// 便于区分「主关联本身就不够用」和「连备用也挂了」
	backupHint := func() string {
		if usingBackup {
			return " (backup providers exhausted)"
		}
		return ""
	}

	// 设置请求超时
	responseHeaderTimeout := time.Second * time.Duration(providersWithMeta.TimeOut)
	// 流式超时时间缩短
	if before.Stream {
		responseHeaderTimeout = responseHeaderTimeout / 3
	}

	authKeyID, _ := ctx.Value(consts.ContextKeyAuthKeyID).(uint)
	authKeyIOLog, _ := ctx.Value(consts.ContextKeyAuthKeyIOLog).(bool)

	traceID, err := token.GenerateRandomChars(10)
	if err != nil {
		return nil, nil, err
	}

	// 会话键在重试循环外解析一次：同一次请求的各次重试共用同一会话��，
	// 避免重试期间会话漂移（随机兜底分支尤其需要）
	sessionKey := ResolveSessionKey(reqMeta.Header, before.SessionID, before.Model, authKeyID, before.raw)

	timer := time.NewTimer(time.Second * time.Duration(providersWithMeta.TimeOut))
	defer timer.Stop()

	// 记录每一轮的真实失败原因：轮次耗尽或候选池提前清空时用它兜底，
	// 否则「provider 配置损坏」这类具体原因会被劣化成笼统的 All retry failed
	var lastErr error
	for retry := range providersWithMeta.MaxRetry {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
			return nil, nil, errors.New("retry time out")
		default:
			// 主池已无可用候选（所有主关联在本请求内都失败出局）→ 降级到备用池。
			// 429 只降权不移出，仍留在主池里继续消耗重试轮次，因此不算「不可用」。
			// 降级只降不升：备用接管后即使主池重新有货也不回退（本请求内主池只会越来越空）。
			if hasBackup && !usingBackup && balancer.Empty() {
				balancer = backupBalancer
				usingBackup = true
				slog.Warn("all primary providers exhausted, falling back to backup providers",
					"model", before.Model, "retry", retry, "traceID", traceID)
			}

			// 加权负载均衡
			id, err := balancer.Pop()
			if err != nil {
				// 池子提前清空时把上一轮的真实原因一并带出，避免被误读成「没有配置关联模型」
				if lastErr != nil {
					return nil, nil, fmt.Errorf("balancer pop err: %v, last error: %w, traceID: %s%s", err, lastErr, traceID, backupHint())
				}
				return nil, nil, fmt.Errorf("balancer pop err: %v, traceID: %s%s", err, traceID, backupHint())
			}

			modelWithProvider, ok := providersWithMeta.ModelWithProviderMap[id]
			if !ok {
				// 数据不一致，移除该模型避免下次重复命中
				balancer.Delete(id)
				continue
			}

			provider := providerMap[modelWithProvider.ProviderID]

			// 日志构造提前到 provider 构建之前：provider 构建失败同样要留痕，
			// 否则这一轮既没有重试日志也没有错误记录，问题无从追查
			log := models.ChatLog{
				Name:           before.Model,
				TraceID:        traceID,
				ProviderModel:  modelWithProvider.ProviderModel,
				ProviderName:   provider.Name,
				Backup:         usingBackup,
				Status:         consts.StatusRunning,
				Style:          style,
				UserAgent:      reqMeta.UserAgent,
				RemoteIP:       reqMeta.RemoteIP,
				AuthKeyID:      authKeyID,
				SessionID:      before.SessionID,
				ChatIO:         authKeyIOLog,
				Retry:          retry,
				ProxyTime:      time.Since(start),
				InputPrice:     lo.FromPtrOr(modelWithProvider.InputPrice, 0),
				CacheReadPrice: lo.FromPtrOr(modelWithProvider.CacheReadPrice, 0),
				OutputPrice:    lo.FromPtrOr(modelWithProvider.OutputPrice, 0),
				Currency:       modelWithProvider.Currency,
			}

			chatModel, err := providers.New(provider.Type, provider.Config, provider.Proxy)
			if err != nil {
				// provider 配置损坏（非法 JSON、type 与上游协议不匹配等）不应硬失败整条请求：
				// 记下本轮原因、把该候选清出候选池，让其它关联模型接手
				retryLog <- log.WithError(err)
				lastErr = err
				balancer.Delete(id)
				continue
			}

			client := providers.GetClient(responseHeaderTimeout, provider.Proxy)

			slog.Info("using provider", "provider", provider.Name, "model", modelWithProvider.ProviderModel)

			// 根据请求原始请求头 是否透传请求头 自定义请求头 构建新的请求头
			withHeader := lo.FromPtrOr(modelWithProvider.WithHeader, false)
			headers := BuildHeaders(reqMeta.Header, withHeader, modelWithProvider.CustomerHeaders, before.Stream, HeaderVars{
				Session:       sessionKey,
				SessionID:     before.SessionID,
				Model:         before.Model,
				ProviderModel: modelWithProvider.ProviderModel,
				TraceID:       traceID,
				AuthKeyID:     authKeyID,
			})

			rawBody, err := buildUpstreamBody(before.raw, modelWithProvider.ExtraBody)
			if err != nil {
				retryLog <- log.WithError(err)
				lastErr = err
				balancer.Delete(id)
				continue
			}

			req, err := chatModel.BuildReq(ctx, headers, modelWithProvider.ProviderModel, rawBody)
			if err != nil {
				retryLog <- log.WithError(err)
				lastErr = err
				// 构建请求失败 移除待选
				balancer.Delete(id)
				continue
			}

			res, err := client.Do(req)
			if err != nil {
				retryLog <- log.WithError(err)
				lastErr = err
				// 请求失败 移除待选
				balancer.Delete(id)
				continue
			}

			if res.StatusCode != http.StatusOK {
				byteBody, err := io.ReadAll(res.Body)
				if err != nil {
					slog.Error("read body error", "error", err)
				}
				statusErr := fmt.Errorf("status: %d, body: %s", res.StatusCode, string(byteBody))
				retryLog <- log.WithError(statusErr)
				lastErr = statusErr

				if res.StatusCode == http.StatusTooManyRequests {
					// 达到RPM限制 降低权重
					balancer.Reduce(id)
				} else {
					// 非RPM限制 移除待选
					balancer.Delete(id)
				}
				res.Body.Close()
				continue
			}

			if provider.ErrorMatcher != "" {
				contentType := strings.ToLower(res.Header.Get("Content-Type"))
				// 流式正常返回通常是 text/event-stream，不提前消费响应体避免影响转发。
				if !strings.Contains(contentType, "text/event-stream") {
					byteBody, err := io.ReadAll(res.Body)
					if err != nil {
						readErr := fmt.Errorf("read body failed: %w", err)
						retryLog <- log.WithError(readErr)
						lastErr = readErr
						balancer.Delete(id)
						res.Body.Close()
						continue
					}

					if matched, sample := matchProviderBodyError(string(byteBody), provider.ErrorMatcher); matched {
						matchErr := fmt.Errorf("response matched provider error sample %q, body: %s", sample, string(byteBody))
						retryLog <- log.WithError(matchErr)
						lastErr = matchErr
						balancer.Delete(id)
						res.Body.Close()
						continue
					}

					res.Body = io.NopCloser(bytes.NewReader(byteBody))
				}
			}

			balancer.Success(id)

			return res, &log, nil
		}
	}

	if lastErr != nil {
		return nil, nil, fmt.Errorf("All retry failed: %w, trace ID: %s%s", lastErr, traceID, backupHint())
	}
	return nil, nil, fmt.Errorf("All retry failed, trace ID: %s%s", traceID, backupHint())
}

func buildUpstreamBody(raw []byte, extraBody map[string]any) ([]byte, error) {
	rawBody := raw
	if len(extraBody) > 0 {
		for key, value := range extraBody {
			var err error
			rawBody, err = sjson.SetBytes(rawBody, key, value)
			if err != nil {
				slog.Warn("failed to set extra body key", "key", key, "error", err)
			}
		}
	}

	rawBody, err := sjson.DeleteBytes(rawBody, "session_id")
	if err != nil {
		return nil, fmt.Errorf("delete session_id from upstream body: %w", err)
	}
	return rawBody, nil
}

func RecordRetryLog(ctx context.Context, retryLog chan models.ChatLog) {
	for log := range retryLog {
		if _, err := SaveChatLog(ctx, log); err != nil {
			slog.Error("save chat log error", "error", err)
		}
	}
}

func RecordLog(ctx context.Context, reqStart time.Time, reader io.ReadCloser, processer Processer, logId uint, before Before, ioLog bool) {
	recordFunc := func() error {
		defer reader.Close()
		if ioLog {
			if err := gorm.G[models.ChatIO](models.DB).Create(ctx, &models.ChatIO{
				Input: string(before.raw),
				LogId: logId,
			}); err != nil {
				return err
			}
		}
		log, output, err := processer(ctx, reader, before.Stream, reqStart)
		if err != nil {
			return err
		}
		log.Status = consts.StatusSuccess
		// processer 返回的是只含耗时/用量/大小的空壳 log：ProviderName / Style / TraceID 等
		// 能保留下来，靠的就是 GORM struct Updates 跳过零值字段。Backup 同理（随 insert 落库），
		// 若这里改成 Save / Select("*") 会把备用标记擦成 false。
		if _, err := gorm.G[models.ChatLog](models.DB).Where("id = ?", logId).Updates(ctx, *log); err != nil {
			return err
		}
		if ioLog {
			if _, err := gorm.G[models.ChatIO](models.DB).Where("log_id = ?", logId).Updates(ctx, models.ChatIO{OutputUnion: *output}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := recordFunc(); err != nil {
		if _, err := gorm.G[models.ChatLog](models.DB).Where("id = ?", logId).Updates(ctx, models.ChatLog{
			Status: consts.StatusError,
			Error:  err.Error(),
		}); err != nil {
			slog.Error("record log error", "error", err)
		}
	}
}

func SaveChatLog(ctx context.Context, log models.ChatLog) (uint, error) {
	if err := gorm.G[models.ChatLog](models.DB).Create(ctx, &log); err != nil {
		return 0, err
	}
	return log.ID, nil
}

func BuildHeaders(source http.Header, withHeader bool, customHeaders map[string]string, stream bool, vars HeaderVars) http.Header {
	header := http.Header{}
	if withHeader {
		header = source.Clone()
	}

	if stream {
		header.Set("X-Accel-Buffering", "no")
	}

	header.Del("Authorization")
	header.Del("X-Api-Key")
	header.Del("X-Goog-Api-Key")

	for key, value := range customHeaders {
		// 支持 {{...}} 占位符动态求值（如会话键）；字面量值行为不变
		rendered := renderHeaderValue(value, vars)
		if rendered == "" {
			// 已配置的自定义头必须给出非空值：部分上游（如 opencode）缺少该头
			// 或值为空都会直接返回 400，空值等同于未配置
			rendered = newRandomID()
		}
		header.Set(key, rendered)
	}

	// Accept-Encoding 必须留给 Go Transport 自行协商：一旦该头被显式设置（透传客户端头或自定义头），
	// Transport 就不再透明解压上游响应，压缩后的字节会被原样记录进 ChatIO（乱码），
	// 同时 usage 解析失败导致 token 统计为 0。放在自定义头之后删除，避免配置重新引入该问题。
	header.Del("Accept-Encoding")

	return header
}

type ProvidersWithMeta struct {
	ModelWithProviderMap map[uint]models.ModelWithProvider
	WeightItems          map[uint]int
	BackupWeightItems    map[uint]int
	ProviderMap          map[uint]models.Provider
	MaxRetry             int
	TimeOut              int
	Strategy             string
	Breaker              bool
}

// splitWeightItems 把过滤后的关联按主/备用拆成两个独立权重池。
// 备用关联不参与日常轮询，只有主池在本请求内被清空后才会被启用；
// 未命中 providerMap 的关联（供应商协议与入站格式不匹配）两池都不进。
func splitWeightItems(modelWithProviders []models.ModelWithProvider, providerMap map[uint]models.Provider) (map[uint]int, map[uint]int) {
	weightItems := make(map[uint]int)
	backupWeightItems := make(map[uint]int)
	for _, mp := range modelWithProviders {
		if _, ok := providerMap[mp.ProviderID]; !ok {
			continue
		}
		if lo.FromPtrOr(mp.Backup, false) {
			backupWeightItems[mp.ID] = mp.Weight
			continue
		}
		weightItems[mp.ID] = mp.Weight
	}
	return weightItems, backupWeightItems
}

func ProvidersWithMetaBymodelsName(ctx context.Context, style string, before Before) (*ProvidersWithMeta, error) {
	model, err := gorm.G[models.Model](models.DB).Where("name = ?", before.Model).First(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if _, err := SaveChatLog(ctx, models.ChatLog{
				Name:      before.Model,
				Status:    consts.StatusError,
				Style:     style,
				SessionID: before.SessionID,
				Error:     err.Error(),
			}); err != nil {
				return nil, err
			}
			return nil, errors.New("not found model " + before.Model)
		}
		return nil, err
	}

	modelWithProviderChain := gorm.G[models.ModelWithProvider](models.DB).Where("model_id = ?", model.ID).Where("status = ?", true)

	if before.toolCall {
		modelWithProviderChain = modelWithProviderChain.Where("tool_call = ?", true)
	}

	if before.structuredOutput {
		modelWithProviderChain = modelWithProviderChain.Where("structured_output = ?", true)
	}

	if before.image {
		modelWithProviderChain = modelWithProviderChain.Where("image = ?", true)
	}

	modelWithProviders, err := modelWithProviderChain.Find(ctx)
	if err != nil {
		return nil, err
	}

	if len(modelWithProviders) == 0 {
		return nil, errors.New("not provider for model " + before.Model)
	}

	modelWithProviderMap := lo.KeyBy(modelWithProviders, func(mp models.ModelWithProvider) uint { return mp.ID })

	providers, err := gorm.G[models.Provider](models.DB).
		Where("id IN ?", lo.Map(modelWithProviders, func(mp models.ModelWithProvider, _ int) uint { return mp.ProviderID })).
		Where("type = ?", style).
		Find(ctx)
	if err != nil {
		return nil, err
	}

	providerMap := lo.KeyBy(providers, func(p models.Provider) uint { return p.ID })

	weightItems, backupWeightItems := splitWeightItems(modelWithProviders, providerMap)

	return &ProvidersWithMeta{
		ModelWithProviderMap: modelWithProviderMap,
		WeightItems:          weightItems,
		BackupWeightItems:    backupWeightItems,
		ProviderMap:          providerMap,
		MaxRetry:             model.MaxRetry,
		TimeOut:              model.TimeOut,
		Strategy:             model.Strategy,
		Breaker:              lo.FromPtrOr(model.Breaker, false),
	}, nil
}
