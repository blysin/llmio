package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/atopos31/llmio/balancers"
	"github.com/atopos31/llmio/bridge"
	"github.com/atopos31/llmio/consts"
	"github.com/atopos31/llmio/models"
	"github.com/atopos31/llmio/pkg/token"
	"github.com/atopos31/llmio/providers"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

// retryLogEntry 是失败重试日志的载体：除了 ChatLog 本身，还要把**这次请求的原始请求体**
// 一并带上，以便在 ioLog 开关打开时把请求体写进 chat_ios.input（不写响应）。
type retryLogEntry struct {
	Log models.ChatLog
	Raw []byte
}

// 撞锁重试的三个常数。
//
// 一次 SQLITE_BUSY 至少要等满驱动的 busy_timeout（5000ms）才会返回，所以次数
// 不能多：这些重试都跑在 RecordLog 的后台 goroutine 里，不占请求的连接。
// 退避用 attempt 的线性倍数——撞锁通常是"另一笔写还没提交"，几十毫秒就够。
const (
	ioCreateAttempts   = 3
	logUpdateAttempts  = 3
	transientRetryBase = 150 * time.Millisecond
)

// sleepBackoff 是撞锁后的退避，可被 ctx 提前打断。
func sleepBackoff(ctx context.Context, attempt int) {
	d := transientRetryBase * time.Duration(attempt)
	if d > time.Second {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// createChatIO 落一条 IO 记录。撞锁时**退回明文**重试。
//
// 压缩是优化、IO 记录是观测，两者都不该让一次成功的聊天变成失败：
// 第一次按正常形态写（可能走块表），之后每次都带 SkipCompress——明文行没有块表
// 写入，事务小得多，在锁竞争里活下来的概率高得多。丢掉的只是压缩率。
func createChatIO(ctx context.Context, logId uint, raw []byte) error {
	var lastErr error
	for attempt := 0; attempt < ioCreateAttempts; attempt++ {
		if attempt > 0 {
			sleepBackoff(ctx, attempt)
		}
		rec := &models.ChatIO{
			Input:        models.BodyBytes(raw),
			LogId:        logId,
			SkipCompress: attempt > 0,
		}
		err := gorm.G[models.ChatIO](models.DB).Create(ctx, rec)
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsBusyErr(err) {
			return err
		}
		slog.Warn("chat io 撞锁，退回明文重试", "attempt", attempt+1, "error", err)
	}
	return lastErr
}

// writeFailedIO 在 ioLog 开关打开时把请求体写进 chat_ios.input（不写响应）。
// IO 记录是观测性功能，写入失败仅记日志，不传播为请求失败。
func writeFailedIO(ctx context.Context, logId uint, raw []byte, ioLog bool) {
	if !ioLog {
		return
	}
	if err := createChatIO(ctx, logId, raw); err != nil {
		slog.Error("save failed io error", "error", err)
	}
}

// saveFailedLog 为"未进入 BalanceChat"的失败路径（模型不存在、无可用提供商）落一条
// error 日志，并在 ioLog 开关打开时写 ChatIO.Input。返回新日志 ID。
func saveFailedLog(ctx context.Context, style string, before Before, sessionID, errMsg string) (uint, error) {
	authKeyID, _ := ctx.Value(consts.ContextKeyAuthKeyID).(uint)
	authKeyIOLog, _ := ctx.Value(consts.ContextKeyAuthKeyIOLog).(bool)
	logId, err := SaveChatLog(ctx, models.ChatLog{
		Name:      before.Model,
		Status:    consts.StatusError,
		Style:     style,
		SessionID: sessionID,
		AuthKeyID: authKeyID,
		ChatIO:    authKeyIOLog,
		Error:     errMsg,
	})
	if err != nil {
		return 0, err
	}
	writeFailedIO(ctx, logId, before.raw, authKeyIOLog)
	return logId, nil
}

func BalanceChat(ctx context.Context, start time.Time, style string, before Before, providersWithMeta ProvidersWithMeta, reqMeta models.ReqMeta, bridgeNotes *BridgeNotes) (*http.Response, *models.ChatLog, error) {
	slog.Info("request", "model", before.Model, "stream", before.Stream, "tool_call", before.toolCall, "structured_output", before.structuredOutput, "image", before.image)

	providerMap := providersWithMeta.ProviderMap

	// 收集重试过程中的err日志。容量按"每轮最多 1+MaxDrops 条"放大：自愈内层
	// 每次失败也走 fail 推一条，通道打满会阻塞转发路径。
	retryLog := make(chan retryLogEntry, providersWithMeta.MaxRetry*(1+SelfHealMaxDrops))
	defer close(retryLog)

	go RecordRetryLog(context.Background(), retryLog)

	authKeyID, _ := ctx.Value(consts.ContextKeyAuthKeyID).(uint)
	authKeyIOLog, _ := ctx.Value(consts.ContextKeyAuthKeyIOLog).(bool)

	// 会话键与路由键在重试循环外解析一次：同一次请求的各次重试共用同一路由键，
	// 避免重试期间会话漂移（随机兜底分支尤其需要）。
	sessionKey, routingKey, stable := resolveSessionKeys(reqMeta.Header, before.SessionID, before.Model, authKeyID, before.raw)
	// 日志列只写稳定的会话键；随机/项目兜底不落库，避免出现一次性值。
	logSessionID := ""
	if stable {
		logSessionID = sessionKey
	}

	// 选择负载均衡策略
	// 主池与备用池各自独立构建：备用关联不参与日常轮询，
	// 只有主池在本请求内被清空（所有主关联都失败出局）后才会接管，且只降不升。
	newBalancer := func(items map[uint]int) balancers.Balancer {
		var b balancers.Balancer
		switch providersWithMeta.Strategy {
		case consts.BalancerRotor:
			b = balancers.NewRotor(items)
		case consts.BalancerSessionAffinity:
			b = balancers.NewSessionAffinity(items, routingKey)
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

	traceID, err := token.GenerateRandomChars(10)
	if err != nil {
		return nil, nil, err
	}

	timer := time.NewTimer(time.Second * time.Duration(providersWithMeta.TimeOut))
	defer timer.Stop()

	// 记录每一轮的真实失败原因：轮次耗尽或候选池提前清空时用它兜底，
	// 否则「provider 配置损坏」这类具体原因会被劣化成笼统的 All retry failed
	var lastErr error
	for retry := range providersWithMeta.MaxRetry {
		select {
		case <-ctx.Done():
			// 走到这里说明请求 context 已经取消：客户端断开，或中间代理掐了连接。
			// 裸的 ctx.Err() 只有一句 "context canceled"，打上阶段再往上抛。
			return nil, nil, MarkStage(StageUpstreamRequest, ctx, ctx.Err())
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
				msg := fmt.Sprintf("balancer pop err: %v, traceID: %s%s", err, traceID, lastErrorSuffix(lastErr)+backupHint())
				if _, lerr := saveFailedLog(ctx, style, before, logSessionID, msg); lerr != nil {
					slog.Error("save failed log error", "error", lerr)
				}
				return nil, nil, errors.New(msg)
			}

			modelWithProvider, ok := providersWithMeta.ModelWithProviderMap[id]
			if !ok {
				// 数据不一致，移除该模型避免下次重复命中
				balancer.Delete(id)
				continue
			}

			provider := providerMap[modelWithProvider.ProviderID]

			// 每换一家上游，记账从头计：上一家的记账若留着，最终那条日志就会记着
			// 产出这次响应的那家**没做过**的改写
			bridgeNotes.reset()

			// 客户端协议与上游协议不同时插一层翻译。候选池里可能混着两种协议的上游
			// （见 ProvidersWithMetaBymodelsName），所以每次 Pop 之后重新判一次方向
			translator := TranslatorFor(style, provider.Type)
			if translator != nil {
				slog.Info("bridging protocols", "from", style, "to", provider.Type, "provider", provider.Name)
			}

			// 分时段计价：按请求发起时刻 + **这一条关联的**峰谷条款解析出
			// 生效单价并快照进日志。快照的是实际生效价，因此历史成本不会被
			// 后来的时段配置改动所改写。
			inputPrice, cacheReadPrice, outputPrice, peakPeriod := ResolvePricingFor(
				ctx, start, modelWithProvider.Peak,
				lo.FromPtrOr(modelWithProvider.InputPrice, 0),
				lo.FromPtrOr(modelWithProvider.CacheReadPrice, 0),
				lo.FromPtrOr(modelWithProvider.OutputPrice, 0),
			)

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
				UpstreamStyle:  provider.Type,
				UserAgent:      reqMeta.UserAgent,
				RemoteIP:       reqMeta.RemoteIP,
				AuthKeyID:      authKeyID,
				SessionID:      logSessionID,
				ChatIO:         authKeyIOLog,
				Retry:          retry,
				ProxyTime:      time.Since(start),
				InputPrice:     inputPrice,
				CacheReadPrice: cacheReadPrice,
				OutputPrice:    outputPrice,
				Currency:       modelWithProvider.Currency,
				PeakPeriod:     peakPeriod,
			}
			// 这次尝试失败的记录。记账要在这里快照：下一次尝试开头会把盒子清空，
			// 而"这一家失败时我们丢了什么"正是排查重试时要看的东西。
			// lastErr 一并收下：全部尝试耗尽后客户端只拿得到一句 "All retry failed"，
			// 真实原因（预检拒绝的原文、上游的 400 报文）必须跟着透出去，否则排查
			// 只能进库里翻 chat_logs.error。
			fail := func(err error) {
				lastErr = err
				log.BridgeNotes = bridgeNotes.String()
				retryLog <- retryLogEntry{Log: log.WithError(err), Raw: before.raw}
			}

			chatModel, err := providers.New(provider.Type, provider.Config, provider.Proxy)
			if err != nil {
				// provider 配置损坏（非法 JSON、type 与上游协议不匹配等）不应硬失败整条请求：
				// 记下本轮原因、把该候选清出候选池，让其它关联模型接手
				fail(err)
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

			// 请求体：先按上游协议翻译，再套上关联行的 extra_body。
			// 顺序不能反——extra_body 挂在这一条关联上，是按**上游协议**写的；先套再翻
			// 的话，翻译层会把这些键当不认识的字段丢掉
			reqBody := before.raw
			if translator != nil {
				converted, notes, err := translator.Request(reqBody, bridge.Options{})
				if err != nil {
					// 翻不过去 = 这家上游服务不了。与 BuildReq 失败同一条路：记账、
					// 移出待选、换下一家
					fail(fmt.Errorf("bridge request: %w", err))
					balancer.Delete(id)
					continue
				}
				bridgeNotes.record(notes)
				reqBody = converted
			}

			rawBody, err := buildUpstreamBody(reqBody, modelWithProvider.ExtraBody)
			if err != nil {
				fail(err)
				balancer.Delete(id)
				continue
			}

			// 强制思考等级：这条关联上配了值就把客户端传的思考等级顶掉。
			// 位置在协议翻译与 extra_body 之后——翻译层跨协议时会丢弃 thinking
			// 相关字段（bridge/request.go），在它之前注入等于白写；而 extra_body
			// 是用户手写的原始键，语义字段应当后落地、覆盖它。
			// 取值来自**这一条关联**而不是模型：同一个模型挂在不同网关上时能吃的
			// 档位并不一样，所以换一家上游就换一份配置。
			// 上游协议不在白名单里时原样返回，见 reasoningEffortKeys。
			beforeEffort := rawBody
			rawBody = applyReasoningEffort(rawBody, provider.Type, modelWithProvider.ReasoningEffort)

			// ---- 内层自愈闭环（计划 §3.5）：上游点名"不认识的参数"时，从实发
			// 请求体删掉该参数、同一家立即重发。改的是实发字节，不重跑翻译与
			// extra_body（同一家上游的翻译产物是确定的）；drops 随本轮外层迭代
			// 生命周期，不跨家携带——A 家不收的参数 B 家可能收，每家用一次 400
			// 换判据，代价可接受。自愈成功不触发 Delete，不污染熔断计数。
			selfHeal := GetSelfHealPolicy(ctx)
			maxDrops := selfHeal.MaxDrops
			if maxDrops <= 0 {
				maxDrops = SelfHealMaxDrops
			}
			drops := map[string]bool{}
			healed := 0
			downgraded := false
			// 强制等级是否真的改动了实发 body：白名单外的协议、写失败、没配置、
			// 以及配的值与客户端本来就传的一样（字节没变），都算没注入。专属
			// 降级只对"我们塞进去的键"负责，客户端自己传的非法档位不归它管
			effortInjected := !bytes.Equal(beforeEffort, rawBody)
			effortDowngraded := false

			var res *http.Response
			nextProvider := false // 跳出内层后是否换下一家
			for {
				// 自愈的多发也受总超时约束：timer 是全请求预算
				select {
				case <-ctx.Done():
					return nil, nil, MarkStage(StageUpstreamRequest, ctx, ctx.Err())
				case <-timer.C:
					return nil, nil, errors.New("retry time out")
				default:
				}

				// 每次都重建请求：删参改的是 rawBody，BuildReq 必须带着新字节重打
				req, err := chatModel.BuildReq(ctx, headers, modelWithProvider.ProviderModel, rawBody)
				if err != nil {
					fail(err)
					// 构建请求失败 移除待选
					balancer.Delete(id)
					nextProvider = true
					break
				}

				res, err = client.Do(req)
				if err != nil {
					// client.Do 失败常常是裸的 "context canceled"：客户端一断开，绑定在
					// 请求 ctx 上的上游调用会被一并取消。打上阶段与归因后再落库，
					// 否则日志里只剩下六个字母，看不出是客户端跑了还是上游没连上。
					fail(MarkStage(StageUpstreamRequest, ctx, err))
					// 请求失败 移除待选
					balancer.Delete(id)
					nextProvider = true
					break
				}

				if res.StatusCode == http.StatusOK {
					break
				}

				byteBody, rerr := io.ReadAll(res.Body)
				res.Body.Close()
				if rerr != nil {
					slog.Error("read body error", "error", rerr)
				}

				// 自愈判定：判据三层全过（状态码 + 短语提取 + 四道闸）才删参
				if selfHeal.Enabled && healed < maxDrops && healableStatusCodes[res.StatusCode] {
					if path, ok := DetectParamComplaint(res.StatusCode, byteBody); ok && PlanDrop(rawBody, path, drops) {
						drops[path] = true
						healed++
						rawBody = ApplyDrop(rawBody, path)
						bridgeNotes.record([]bridge.Note{bridge.Note(SelfHealNotePrefix + path)})
						// 自愈也是一次真实上游调用，失败行与最终成功行都如实计数
						log.Retry = retry + healed
						fail(fmt.Errorf("status: %d, body: %s (self-heal: dropped %s)", res.StatusCode, string(byteBody), path))
						continue
					}
				}

				// 强制思考等级的专属降级：上游点名拒收思考等级（参数或值）时，
				// 删掉注入的键重发同一家。与通用删参分开是因为这类报文多是
				// **值错误**（Unsupported value），短语表不认；独立预算每家一次，
				// 不占删参额度。降级后回到"不干预"语义，请求本身不挂
				if selfHeal.Enabled && effortInjected && !effortDowngraded &&
					DetectEffortComplaint(res.StatusCode, byteBody) {
					if next, dropped := DropReasoningEffort(rawBody); dropped {
						effortDowngraded = true
						rawBody = next
						bridgeNotes.record([]bridge.Note{bridge.Note(EffortDowngradeNote)})
						log.Retry = retry + healed + 1
						fail(fmt.Errorf("status: %d, body: %s (downgrade: forced reasoning effort dropped)", res.StatusCode, string(byteBody)))
						continue
					}
				}

				// 工具内容数组的运行时降级：搬运过去的块数组被上游以类型错点名时，
				// 压回纯文本（丢图、留文本）重发。与删参同一认识论——先发完整形状，
				// 被点名拒收才降级且留痕；一条独立于删参预算（每家最多一次）
				if selfHeal.Enabled && !downgraded && healableStatusCodes[res.StatusCode] &&
					toolContentArrayPattern.MatchString(complaintText(byteBody)) {
					if next, droppedImgs := FlattenToolContent(rawBody); droppedImgs >= 0 && !bytes.Equal(next, rawBody) {
						downgraded = true
						rawBody = next
						if droppedImgs > 0 {
							bridgeNotes.record([]bridge.Note{bridge.NoteDroppedToolResultImage})
						}
						log.Retry = retry + healed + lo.Ternary(effortDowngraded, 1, 0) + 1
						fail(fmt.Errorf("status: %d, body: %s (downgrade: tool content to text, %d image(s) dropped)", res.StatusCode, string(byteBody), droppedImgs))
						continue
					}
				}

				fail(fmt.Errorf("status: %d, body: %s", res.StatusCode, string(byteBody)))

				if res.StatusCode == http.StatusTooManyRequests {
					// 达到RPM限制 降低权重
					balancer.Reduce(id)
				} else {
					// 非RPM限制 移除待选
					balancer.Delete(id)
				}
				res = nil
				nextProvider = true
				break
			}
			if nextProvider {
				continue
			}

			if provider.ErrorMatcher != "" {
				contentType := strings.ToLower(res.Header.Get("Content-Type"))
				// 流式正常返回通常是 text/event-stream，不提前消费响应体避免影响转发。
				if !strings.Contains(contentType, "text/event-stream") {
					byteBody, err := io.ReadAll(res.Body)
					if err != nil {
						fail(fmt.Errorf("read body failed: %w", err))
						balancer.Delete(id)
						res.Body.Close()
						continue
					}

					if matched, sample := matchProviderBodyError(string(byteBody), provider.ErrorMatcher); matched {
						fail(fmt.Errorf("response matched provider error sample %q, body: %s", sample, string(byteBody)))
						balancer.Delete(id)
						res.Body.Close()
						continue
					}

					res.Body = io.NopCloser(bytes.NewReader(byteBody))
				}
			}

			// 响应体：翻成客户端协议之后再交出去。必须在转发与记录**之前**——
			// 客户端看到的和 ChatLog 记的都得是客户端协议（记录用的 processer 是按
			// 客户端协议选的）
			if translator != nil {
				if err := bridgeResponse(translator, res, before.Stream, bridgeNotes); err != nil {
					fail(fmt.Errorf("bridge response: %w", err))
					balancer.Delete(id)
					res.Body.Close()
					continue
				}
			}

			balancer.Success(id)

			return res, &log, nil
		}
	}

	return nil, nil, fmt.Errorf("All retry failed, trace ID: %s%s", traceID, lastErrorSuffix(lastErr)+backupHint())
}

// lastErrorSuffix 把最后一次失败的原因拼进收尾错误。截断 500 字符，与
// ClassifyError 对 raw 的截断同款——错误体可能是整份上游报文，不截会爆破
// 客户端的 500 响应。没有失败过（首轮就池空）时返回空串。
func lastErrorSuffix(lastErr error) string {
	if lastErr == nil {
		return ""
	}
	return ", last error: " + Truncate(lastErr.Error(), 500)
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

// reasoningEffortKeys 把"思考等级"落到各家上游协议里的请求体路径。
//
// 只有 OpenAI 系的两种：chat completions 认顶层的 reasoning_effort，Responses
// API 把它收进 reasoning.effort。这两家说的是同一个东西，只是键名不同。
//
// 刻意**不**包含 Anthropic 与 Gemini：那两家要的是 token 预算（thinking.budget_tokens
// / thinkingBudget），与"等级"不是一回事。真要做就是一张等级→预算的映射表，
// 那是另一个需求，不该以一个字符串字段的名义偷偷塞进来。
var reasoningEffortKeys = map[string]string{
	consts.StyleOpenAI:    "reasoning_effort",
	consts.StyleOpenAIRes: "reasoning.effort",
}

// applyReasoningEffort 用关联上配置的等级覆盖客户端请求体里的思考等级。
//
// effort 为空表示不干预，直接返回原字节（这是存量模型的常态，零开销）。
// 上游协议不在白名单里也原样返回：配置是模型级的，同一个模型下可能同时挂着
// OpenAI 与 Anthropic 的上游，后者收不了这个键，写进去只会换一个 400。
//
// 写失败只记日志不中断——思考等级是增强项，不是路由决策。上游若明确拒收这个
// 参数或档位值，专属降级（DetectEffortComplaint + DropReasoningEffort）会把它
// 删掉后重发同一家，请求本身不会挂。不走通用删参：值错误（Unsupported value）
// 的措辞短语表不认，见 effortComplaintPattern。
func applyReasoningEffort(rawBody []byte, upstreamStyle, effort string) []byte {
	if effort == "" {
		return rawBody
	}
	path, ok := reasoningEffortKeys[upstreamStyle]
	if !ok {
		return rawBody
	}
	next, err := sjson.SetBytes(rawBody, path, effort)
	if err != nil {
		slog.Warn("force reasoning effort failed",
			"style", upstreamStyle, "path", path, "effort", effort, "error", err)
		return rawBody
	}
	// Responses API 的请求体里不该再有 chat completions 的顶层键：客户端从
	// chat 请求改造过来的 body 常带着它。两个键并存时宽松网关取哪个说不准
	// （强制可能静默失效），严格网关直接 400。写进 reasoning.effort 的同时
	// 把旧键删掉，实发请求里思考等级只有一处来源。
	if upstreamStyle == consts.StyleOpenAIRes {
		if cleaned, derr := sjson.DeleteBytes(next, "reasoning_effort"); derr == nil {
			next = cleaned
		}
	}
	// 覆盖是静默的：客户端以为自己指定了 low、实际发出去的是 high，出问题时
	// 只能靠这条日志还原现场（响应里看不出任何痕迹）
	slog.Info("forced reasoning effort", "style", upstreamStyle, "effort", effort)
	return next
}

// EffortDowngradeNote 强制思考等级被上游拒收后降级删除的记账短码。
// 与自愈删参（self_heal_dropped:*）、工具内容降级（dropped_tool_result_image）
// 同一族：请求侧被点名拒收才降级，且必须留痕。
const EffortDowngradeNote = "dropped_forced_reasoning_effort"

// effortComplaintPattern 识别上游对"思考等级"本身的拒绝——参数名或值被点名。
// 典型报文（原生 OpenAI，模型不支持思考时）：
//
//	Unsupported value: 'reasoning_effort' does not support 'max' with this model.
//
// 这类**值错误**通用自愈的短语表（paramComplaintPatterns）一条都不认——那张表
// 只管"参数不认识"。而强制档位最常见的失败恰恰是"参数认识、值/模型不认"，
// 没有这条专属规则，配错档位的关联每条请求都会 400 后换家甚至整单失败。
// 引号与前后边界都得放宽：真实报文并不统一用引号（vLLM/OpenRouter 一类写的是
// "Unrecognized request argument supplied: reasoning.effort"，裸键无引号），
// 而 Go 的 RE2 没有 lookaround，边界只能靠"前一个非词字符 + 后一个非词字符"
// 这种双向夹逼表达，顺便把 reasoning_effort_v2 这类同前缀的无关参数挡在外面。
var effortComplaintPattern = regexp.MustCompile(
	`(?i)(?:^|[^\w.])['"\x60]?(?:reasoning_effort|reasoning\.effort)['"\x60]?(?:$|[^\w])`)

// DetectEffortComplaint 判定一条上游错误是否点名了思考等级键。只在强制等级
// 真的注入了这份请求体时才该被调用（见 chat.go 接线处的 effortInjected 闸）：
// 客户端自己传的非法档位不归这条规则管，那是通用自愈/换家的地盘。
func DetectEffortComplaint(status int, body []byte) bool {
	if !healableStatusCodes[status] {
		return false
	}
	return effortComplaintPattern.MatchString(complaintText(body))
}

// DropReasoningEffort 把思考等级键从实发请求体里删干净：顶层 reasoning_effort
// 与 Responses 的 reasoning.effort 都删——上游点名的可能是其中任何一个，而这份
// body 里两处都可能有值（见 applyReasoningEffort 的双写清理）。两个键都不在时
// 返回原字节与 false，调用方据此不空转重发。
func DropReasoningEffort(raw []byte) ([]byte, bool) {
	if !gjson.GetBytes(raw, "reasoning_effort").Exists() &&
		!gjson.GetBytes(raw, "reasoning.effort").Exists() {
		return raw, false
	}
	out := raw
	if next, err := sjson.DeleteBytes(out, "reasoning_effort"); err == nil {
		out = next
	}
	if next, err := sjson.DeleteBytes(out, "reasoning.effort"); err == nil {
		out = next
	}
	return out, true
}

func RecordRetryLog(ctx context.Context, retryLog chan retryLogEntry) {
	for entry := range retryLog {
		log := entry.Log
		logId, err := SaveChatLog(ctx, log)
		if err != nil {
			slog.Error("save chat log error", "error", err)
			continue
		}
		writeFailedIO(ctx, logId, entry.Raw, log.ChatIO)
	}
}

// RecordLog 是请求的**记录路径**：落 IO、读响应、把结果写回日志。
//
// 它跑在 handler 起的后台 goroutine 里（context.Background），所以这里的任何失败
// 都不该反过来影响已经发给客户端的响应。2026-10-17 线上教训：数据库被锁住时，
// 这里在落 ChatIO 那一步就把错误抛给"把请求标成 error"的分支，于是**成功的聊天
// 全被记成失败**；结束时的 UPDATE 又跟着失败，那些行就永远停在 running。
//
// 现在三条规矩：
//  1. IO 记录失败只降级（createChatIO 内部先退回明文重试），不改请求结论；
//  2. 只有"读响应真的失败"才把日志标 error；
//  3. 落库失败按撞锁重试——日志宁可晚到，不要永远在途。
func RecordLog(ctx context.Context, reqStart time.Time, reader io.ReadCloser, processer Processer, logId uint, before Before, ioLog bool, bridgeNotes *BridgeNotes) {
	defer reader.Close()

	ioStored := false
	if ioLog {
		// BodyBytes 不做拷贝：before.raw 由本请求独占，而 Create 是同步的，
		// 等它读完才会继续往下走。真正的分块与压缩都在 BeforeCreate 里发生。
		if err := createChatIO(ctx, logId, before.raw); err != nil {
			slog.Warn("save chat io error（本次请求不记录 IO，请求本身不受影响）", "error", err)
		} else {
			ioStored = true
		}
	}

	log, output, err := processer(ctx, reader, before.Stream, reqStart)
	if err != nil {
		// 这里的 err 多半已经被上层（handler 的 io.Copy）打过阶段标记，
		// MarkStage 幂等会原样放行；只有 processer 自己新产生的错误
		// （如上游 SSE 里的 error 字段）才会在这里被补上阶段前缀。
		update := models.ChatLog{
			Status: consts.StatusError,
			Error:  MarkStage(StageResponseStream, ctx, err).Error(),
		}
		// 只补部分进度三列，不整体 Save：结构体 Updates 仍会跳过零值，
		// 因此 Name / ProviderName / TraceID 这些 insert 时写好的字段不会被擦掉。
		// 不接住部分进度的话，「流到一半才断」在日志上会显示成 0 字节 / 0 耗时，
		// 和「一个字节都没收到」完全无从区分。
		if log != nil {
			update.FirstChunkTime = log.FirstChunkTime
			update.ChunkTime = log.ChunkTime
			update.Size = log.Size
		}
		if uerr := updateChatLog(ctx, logId, update); uerr != nil {
			slog.Error("record log error", "error", uerr)
		}
		return
	}

	log.Status = consts.StatusSuccess
	// 协议互转的记账：响应侧要到流读完才完整，所以在这里（日志落库前）取
	log.BridgeNotes = bridgeNotes.String()
	if err := updateChatLog(ctx, logId, *log); err != nil {
		// 连重试都撞锁：这次请求的"成功"结论没落库。**不能留在 running**——
		// 界面上"一直显示在途"就是它；旧版本还会一直留到下次重启。
		// 退一步写 error，并把原因写清楚：请求本身已经完成，只是状态没写进去。
		// 兜底只试一次——前面已经撞满三次 busy_timeout，再久只会拖住这个 goroutine。
		fallback := models.ChatLog{
			Status: consts.StatusError,
			// 落库失败也是一类失败，标上阶段让日志页能和上游/流错误分开。
			Error: MarkStage(StageLogPersist, ctx,
				fmt.Errorf("记录日志失败：请求已完成，但状态写入数据库时撞锁（%s）", err.Error())).Error(),
		}
		if uerr := updateChatLogN(ctx, logId, fallback, 1); uerr != nil {
			slog.Error("record log error（日志仍停在 running，等重启收尾）", "log_id", logId, "error", uerr)
		}
		return
	}
	if ioStored && output != nil {
		if err := updateChatIOOutput(ctx, logId, output); err != nil {
			slog.Warn("record chat io output error", "log_id", logId, "error", err)
		}
	}
}

// updateChatLog 更新一条日志，撞锁重试。
func updateChatLog(ctx context.Context, logId uint, log models.ChatLog) error {
	return updateChatLogN(ctx, logId, log, logUpdateAttempts)
}

// updateChatLogN 是 updateChatLog 的可指定次数版本（兜底路径只需要一次）。
func updateChatLogN(ctx context.Context, logId uint, log models.ChatLog, attempts int) error {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			sleepBackoff(ctx, attempt)
		}
		if _, err := gorm.G[models.ChatLog](models.DB).Where("id = ?", logId).Updates(ctx, log); err != nil {
			lastErr = err
			if !IsBusyErr(err) {
				return err
			}
			continue
		}
		return nil
	}
	return lastErr
}

// updateChatIOOutput 回填响应体，撞锁重试（条数少，一次成功即可）。
func updateChatIOOutput(ctx context.Context, logId uint, output *models.OutputUnion) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			sleepBackoff(ctx, attempt)
		}
		if _, err := gorm.G[models.ChatIO](models.DB).Where("log_id = ?", logId).
			Updates(ctx, models.ChatIO{OutputUnion: *output}); err != nil {
			lastErr = err
			if !IsBusyErr(err) {
				return err
			}
			continue
		}
		return nil
	}
	return lastErr
}

// SaveChatLog 插入一条日志。撞锁重试：这条 INSERT 在响应发出**之前**同步执行，
// 失败会直接把一次正常聊天打成 500。重试时重置 ID，避免失败那次的 LAST_INSERT_ID
// 被带进第二次 INSERT（GORM 会把它当显式主键）。
func SaveChatLog(ctx context.Context, log models.ChatLog) (uint, error) {
	var lastErr error
	for attempt := 0; attempt < logUpdateAttempts; attempt++ {
		if attempt > 0 {
			sleepBackoff(ctx, attempt)
			log.ID = 0
		}
		if err := gorm.G[models.ChatLog](models.DB).Create(ctx, &log); err != nil {
			lastErr = err
			if !IsBusyErr(err) {
				return 0, err
			}
			continue
		}
		return log.ID, nil
	}
	return 0, lastErr
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

func ProvidersWithMetaBymodelsName(ctx context.Context, header http.Header, style string, before Before, at time.Time) (*ProvidersWithMeta, error) {
	authKeyID, _ := ctx.Value(consts.ContextKeyAuthKeyID).(uint)
	logSessionID := stableSessionKey(header, before.SessionID, before.Model, authKeyID, before.raw)

	model, err := gorm.G[models.Model](models.DB).Where("name = ?", before.Model).First(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if _, err := saveFailedLog(ctx, style, before, logSessionID, err.Error()); err != nil {
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
		if _, err := saveFailedLog(ctx, style, before, logSessionID, "not provider for model "+before.Model); err != nil {
			return nil, err
		}
		return nil, errors.New("not provider for model " + before.Model)
	}

	// 可用时间过滤：必须在 splitWeightItems **之前**完成。
	// 放在之后的话，主池被时间窗清空时，备用池里同样在窗口外的关联仍会被
	// 选中，时间窗等于摆设（见 service/availability.go 的规则 2）。
	//
	// at 由调用方传入**请求发起时刻**，而不是这里现取 time.Now()：
	// BalanceChat 里 ResolvePricingFor 用的就是同一个 start，两边取不同
	// 时刻会让跨时段边界的请求出现"按谷时计价、按峰时选上游"这种自相矛盾的组合。
	totalAssocs := len(modelWithProviders)
	calendar := GetPeakCalendar(ctx)
	available, outside, nextOpen := filterByAvailability(modelWithProviders, at, calendar)
	if len(available) == 0 {
		// 与上面"关联一条都没有"刻意区分：那是配置错误，这是**会自行恢复**的
		// 状态。运维看到 "not provider for model" 会去查是不是关联被删了，
		// 而真实原因可能只是"现在 20:47，上游的窗口 18:00 就关了"。
		noAvail := &NoAvailabilityError{
			Model:    before.Model,
			Outside:  outside,
			Total:    totalAssocs,
			At:       at,
			NextOpen: nextOpen,
		}
		if _, err := saveFailedLog(ctx, style, before, logSessionID, noAvail.Error()); err != nil {
			return nil, err
		}
		return nil, noAvail
	}
	modelWithProviders = available

	modelWithProviderMap := lo.KeyBy(modelWithProviders, func(mp models.ModelWithProvider) uint { return mp.ID })

	providers, err := gorm.G[models.Provider](models.DB).
		Where("id IN ?", lo.Map(modelWithProviders, func(mp models.ModelWithProvider, _ int) uint { return mp.ProviderID })).
		Where("type IN ?", ServableTypes(style)).
		Find(ctx)
	if err != nil {
		return nil, err
	}

	// 直连优先，转换为兜底：只要这个模型下有一个说客户端话的上游，候选池就只有它们。
	// 转换只在该模型下"没人说客户端话"时启用，行为可预期；现有部署（上游与客户端
	// 同协议）的候选集也因此与转换功能上线前完全一致。
	// 模型上可以把这条关掉（PreferDirect），关掉就是整池按权重摇
	providers = poolFor(providers, style, model.PreferDirect)

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
