package socks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newTestPath — путь на живой SOCKS-стаб с локальным check-сервером.
// CheckInterval большой, чтобы периодический тикер не вмешивался в сценарий
// (в этих тестах Run не запускается — проверки идут через gate по требованию).
func newTestPath(t *testing.T, name, socksURL, expectedIP string, check *ipCheckServer) *Path {
	t.Helper()
	p, err := New(name, ResolvedConfig{
		URL:           socksURL,
		ExpectedIP:    expectedIP,
		CheckURL:      check.URL(),
		CheckInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("socks.New: %v", err)
	}
	return p
}

func TestRecheckUpdatesLiveStatus(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	p := newTestPath(t, "manual", stub.URL(), "203.0.113.7", check)
	ctx := context.Background()
	if err := p.Recheck(ctx); err != nil || !p.fresh() {
		t.Fatalf("initial check: %v", err)
	}
	check.setStatus(http.StatusServiceUnavailable)
	if err := p.Recheck(ctx); err == nil || p.Status().State != "blocked" {
		t.Fatalf("forced check should invalidate fresh status: %+v (%v)", p.Status(), err)
	}
	check.setStatus(http.StatusOK)
	if err := p.Recheck(ctx); err != nil || p.Status().State != "healthy" || check.hitCount() != 3 {
		t.Fatalf("forced check should bypass blocked cooldown: %+v (%v), hits=%d", p.Status(), err, check.hitCount())
	}
	p.checkMu.Lock()
	defer p.checkMu.Unlock()
	if err := p.Recheck(ctx); !errors.Is(err, ErrCheckInProgress) {
		t.Fatalf("concurrent check: %v", err)
	}
}

func TestCheckPreview(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "2001:db8::1")
	cfg := ResolvedConfig{URL: stub.URL(), CheckURL: check.URL(), ExpectedIP: "2001:0db8:0:0:0:0:0:1"}
	status, err := Check(context.Background(), cfg)
	if err != nil || status.State != "healthy" || status.ActualIP != "2001:db8::1" {
		t.Fatalf("preview did not match equivalent IPv6 address: %+v (%v)", status, err)
	}
	cfg.ExpectedIP = "2001:db8::2"
	status, err = Check(context.Background(), cfg)
	if err == nil || status.State != "blocked" || status.ActualIP != "2001:db8::1" {
		t.Fatalf("preview did not report mismatch: %+v (%v)", status, err)
	}
}

// drain дочитывает и закрывает тело ответа (нужно и для ошибочных ответов).
// Ошибки чтения здесь возможны при отмене запроса — логируем, не роняем.
func drain(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp == nil {
		return
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Logf("чтение тела ответа: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("закрытие тела ответа: %v", err)
	}
}

// doExpectError выполняет запрос и ждёт ошибку, прибирая ответ.
func doExpectError(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	resp, err := c.Get(url)
	if resp != nil {
		drain(t, resp)
	}
	if err == nil {
		t.Fatalf("ожидали ошибку для %s, получили успех", url)
	}
	return err
}

// doExpectOK выполняет запрос и ждёт 200 с телом "ok" (ответ target-стаба).
func doExpectOK(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("запрос %s: %v", url, err)
	}
	defer drain(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d, ожидался 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("тело %q, ожидалось \"ok\"", b)
	}
}

// assertBlockedError проверяет, что err — именно *BlockedError (I4).
func assertBlockedError(t *testing.T, err error) *BlockedError {
	t.Helper()
	if err == nil {
		t.Fatal("ожидали ошибку, получили nil")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("ожидали *socks.BlockedError, получили %T: %v", err, err)
	}
	return be
}

// SPEC 14а: fail-closed на уровне пакета — недоступный SOCKS → BlockedError,
// до апстрима не доходит НИ ОДНОГО коннекта (инварианты I1, I4).
func TestDeadSocksYieldsBlockedErrorAndZeroConnectionsToTarget(t *testing.T) {
	target := newTargetServer(t)
	check := newIPCheckServer(t, "203.0.113.7")

	path, err := New("dead", ResolvedConfig{
		URL:           "socks5h://" + deadAddr(t),
		ExpectedIP:    "203.0.113.7",
		CheckURL:      check.URL(),
		CheckInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("socks.New: %v", err)
	}

	err = doExpectError(t, path.Client(), target.URL())
	be := assertBlockedError(t, err)
	if be.Path != "dead" || be.Reason == "" {
		t.Fatalf("неожиданная BlockedError: %+v", be)
	}
	if n := target.hitCount(); n != 0 {
		t.Fatalf("fail-closed нарушен: апстрим получил %d коннектов, ожидалось 0", n)
	}
	if n := check.hitCount(); n != 0 {
		t.Fatalf("check-сервер недостижим через мёртвый SOCKS, но получил %d хитов", n)
	}
}

// Сетевая ошибка запроса взводит needCheck: следующий запрос сначала проверяет
// путь (SPEC 6 «проверка по триггеру») и только потом идёт к апстриму.
func TestNetworkErrorTriggersCheckBeforeNextRequest(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "nc", stub.URL(), "203.0.113.7", check)

	// 1) Первичный чек + сетевая ошибка: цель — закрытый порт.
	_ = doExpectError(t, path.Client(), "http://"+deadAddr(t)+"/")
	if got := check.hitCount(); got != 1 {
		t.Fatalf("первый запрос должен сперва проверить путь: хитов %d, ожидался 1", got)
	}

	// 2) Следующий запрос к живой цели: gate обязан СНАЧАЛА перепроверить путь.
	doExpectOK(t, path.Client(), target.URL())
	if got := check.hitCount(); got != 2 {
		t.Fatalf("нужна внеплановая проверка после сетевой ошибки: хитов %d, ожидалось 2", got)
	}
	if got := target.hitCount(); got != 1 {
		t.Fatalf("запрос должен дойти до цели после проверки: %d", got)
	}
}

// blocked → чек начинает проходить → путь снова healthy и пропускает трафик.
func TestBlockedPathRecoversAfterSuccessfulCheck(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "rec", stub.URL(), "203.0.113.7", check)
	path.minRecheckGap = 0 // антишторм отключён: проверяем именно recovery, а не гэп

	check.setStatus(http.StatusInternalServerError)
	err := doExpectError(t, path.Client(), target.URL())
	assertBlockedError(t, err)

	check.setStatus(http.StatusOK)
	doExpectOK(t, path.Client(), target.URL())
	if st := path.Status(); st.State != StateHealthy.String() {
		t.Fatalf("после успешного чека путь должен быть healthy: %+v", st)
	}
}

// Антишторм: повторный запрос к blocked-пути не порождает новый чек, пока не
// истёк minRecheckGap — иначе каждый запрос долбил бы мёртвый SOCKS.
func TestBlockedPathIsNotRecheckedWithinMinRecheckGap(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "storm", stub.URL(), "203.0.113.7", check)
	path.minRecheckGap = time.Hour // гэп больше времени теста: второй чек невозможен

	check.setStatus(http.StatusInternalServerError)
	err := doExpectError(t, path.Client(), target.URL())
	assertBlockedError(t, err)

	err = doExpectError(t, path.Client(), target.URL())
	assertBlockedError(t, err)
	if got := check.hitCount(); got != 1 {
		t.Fatalf("антишторм нарушен: второй чек случился (хитов %d, ожидался 1)", got)
	}

	// Гэп истёк (обнулили поле): следующий запрос снова проверяет путь.
	path.minRecheckGap = 0
	err = doExpectError(t, path.Client(), target.URL())
	assertBlockedError(t, err)
	if got := check.hitCount(); got != 2 {
		t.Fatalf("после истечения гэпа нужен новый чек: хитов %d, ожидалось 2", got)
	}
}

// IP-пиннинг: expected_ip совпал с фактическим — путь работает и помнит IP.
func TestIPPinMatchLetsRequestThrough(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "pin", stub.URL(), "203.0.113.7", check)

	doExpectOK(t, path.Client(), target.URL())
	st := path.Status()
	if st.State != StateHealthy.String() {
		t.Fatalf("ожидали healthy: %+v", st)
	}
	if st.ActualIP != "203.0.113.7" {
		t.Fatalf("фактический IP должен зафиксироваться в статусе: %+v", st)
	}
}

// IP-пиннинг: фактический IP ≠ expected_ip → blocked, в ошибке видны оба адреса.
func TestIPPinMismatchBlocksPathWithExpectedAndGot(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "pin", stub.URL(), "1.2.3.4", check)

	err := doExpectError(t, path.Client(), target.URL())
	be := assertBlockedError(t, err)
	if !strings.Contains(be.Error(), "expected 1.2.3.4") || !strings.Contains(be.Error(), "got 203.0.113.7") {
		t.Fatalf("в ошибке должны быть ожидаемый и фактический IP: %v", be)
	}
	if got := target.hitCount(); got != 0 {
		t.Fatalf("запрос при несовпадении IP не должен доходить до апстрима: %d коннектов", got)
	}
}

// Таймаут проверки по дедлайну ЗАПРОСА — это аборт (F1): клиент получает
// fail-closed ошибку быстро, но state пути не портится (родительский ctx мёртв,
// значит результат проверки недостоверен — см. runCheckLocked).
func TestIPPinCheckDeadlineOfRequestReturnsBlockedErrorWithoutStateDamage(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "pin", stub.URL(), "203.0.113.7", check)
	check.setBlock(true)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	start := time.Now()
	resp, err := path.Client().Do(req)
	if resp != nil {
		drain(t, resp)
	}
	be := assertBlockedError(t, err)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("чек завис дольше дедлайна запроса: %v (причина: %s)", elapsed, be.Reason)
	}
	if !strings.Contains(be.Reason, "deadline") {
		t.Fatalf("причиной должна быть просрочка дедлайна: %s", be.Reason)
	}
	if st := path.Status(); st.State != StateUnchecked.String() || st.LastError != "" {
		t.Fatalf("аборт по дедлайну запроса не должен портить state: %+v", st)
	}
	if got := target.hitCount(); got != 0 {
		t.Fatalf("апстрим не должен получить коннектов: %d", got)
	}
}

// F1: отмена ctx периодической проверки в полёте (релоад: old.cancel()) — это
// АБОРТ, а не провал: переиспользованный здоровый путь не должен стать blocked.
func TestRunContextCancelDuringCheckDoesNotBlockPath(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	path := newTestPath(t, "abort", stub.URL(), "203.0.113.7", check)
	check.setBlock(true)                           // хендлер встаёт на паузу и сигналит «чек дошёл»
	path.cfg.CheckInterval = 20 * time.Millisecond // тикер Run дойдёт до проверки быстро

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		path.Run(ctx)
		close(done)
	}()

	check.waitHeld(t) // периодическая проверка началась и висит на медленном чек-сервере
	cancel()          // аналог old.cancel() в Manager.Reload при полётной проверке
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены ctx")
	}

	st := path.Status()
	if st.State != StateUnchecked.String() {
		t.Fatalf("аборт проверки не должен помечать путь blocked: %+v", st)
	}
	if st.LastError != "" {
		t.Fatalf("lastErr должен остаться пустым после аборта: %+v", st)
	}
	if got := check.hitCount(); got != 1 {
		t.Fatalf("ожидался ровно один начавшийся чек: %d", got)
	}
}

// Регресс F1: провал по СОБСТВЕННОМУ дедлайну чека (cctx при живом родителе)
// — это по-прежнему провал: путь блокируется, как раньше.
func TestCheckDeadlineWithLiveParentStillBlocksPath(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "cdead", stub.URL(), "203.0.113.7", check)
	path.checkTimeout = 150 * time.Millisecond // собственный дедлайн чека короче дефолтных 10s
	check.setBlock(true)

	// Родитель (контекст запроса) без дедлайна — жив на момент провала чека.
	err := doExpectError(t, path.Client(), target.URL())
	be := assertBlockedError(t, err)
	if !strings.Contains(be.Reason, "deadline") {
		t.Fatalf("причиной должен быть собственный дедлайн чека: %s", be.Reason)
	}
	if st := path.Status(); st.State != StateBlocked.String() {
		t.Fatalf("провал чека при живом родителе должен блокировать путь: %+v", st)
	}
	if got := target.hitCount(); got != 0 {
		t.Fatalf("запрос не должен доходить до цели: %d", got)
	}
}

// F8: до первой проверки LastCheck пуст, а не "0001-01-01T00:00:00Z".
func TestStatusLastCheckEmptyBeforeFirstCheck(t *testing.T) {
	path, err := New("fresh", ResolvedConfig{
		URL:           "socks5h://127.0.0.1:9",
		CheckInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("socks.New: %v", err)
	}
	if st := path.Status(); st.LastCheck != "" {
		t.Fatalf("до первой проверки LastCheck должен быть пустым: %q", st.LastCheck)
	}
}

// FC-8: отмена контекста запроса — не сетевая ошибка пути; needCheck не
// взводится, следующий запрос идёт без внепланового чека.
func TestRequestContextCancelDoesNotFlagNeedCheck(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "fc8", stub.URL(), "203.0.113.7", check)

	target.setHang(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	type result struct {
		resp *http.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := path.Client().Do(req)
		ch <- result{resp, err}
	}()
	target.waitReached(t) // gate уже пройден, запрос «висит» в хендлере цели
	cancel()
	r := <-ch
	if r.resp != nil {
		drain(t, r.resp)
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ожидали context.Canceled, получили %v", r.err)
	}

	// Если бы отмена репортилась как сетевая ошибка пути, этот запрос сначала
	// пошёл бы на внеплановый чек (hitCount стал бы 2).
	target.setHang(false)
	doExpectOK(t, path.Client(), target.URL())
	if got := check.hitCount(); got != 1 {
		t.Fatalf("отмена контекста не должна взводить needCheck: хитов чека %d, ожидался 1", got)
	}
	if got := target.hitCount(); got != 1 {
		t.Fatalf("второй запрос должен дойти до цели: %d", got)
	}
}

// Direct (socks:"none") — обычный клиент для локальных base_url, gate не нужен.
func TestDirectClientReachesLocalTarget(t *testing.T) {
	target := newTargetServer(t)
	doExpectOK(t, Direct{}.Client(), target.URL())
}

// N1: abort проверки — до старта и в полёте — сдвигает lastTry под антишторм-
// гэп: повторные запросы с отменённым ctx не должны перезапускать чек чаще
// minRecheckGap (доказательство — счётчик чек-сервера).
func TestAbortedCheckCountsAgainstMinRecheckGap(t *testing.T) {
	stub := newSocksStub(t)
	check := newIPCheckServer(t, "203.0.113.7")
	target := newTargetServer(t)
	path := newTestPath(t, "abortgap", stub.URL(), "203.0.113.7", check)
	path.minRecheckGap = time.Hour

	// Переводим путь в blocked: именно в этом состоянии антишторм-ветка
	// ensureUsable смотрит на lastTry.
	check.setStatus(http.StatusInternalServerError)
	assertBlockedError(t, doExpectError(t, path.Client(), target.URL()))
	baseline := check.hitCount()

	// Отмена ctx В ПОЛЁТЕ проверки: чек висит на медленном стабе, клиент
	// отменяет запрос. lastTry должен сдвинуться абортом.
	check.setBlock(true)
	zeroLastTry(path)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := path.Client().Do(req)
		if resp != nil {
			drain(t, resp)
		}
		done <- err
	}()
	check.waitHeld(t) // чек дошёл до стаба и висит
	cancel()
	if err := <-done; err == nil {
		t.Fatal("ожидали ошибку aborted")
	}
	if got := check.hitCount(); got != baseline+1 {
		t.Fatalf("в полёте должен был начаться ровно один чек: %d, ожидалось %d", got, baseline+1)
	}

	// Повторный запрос в пределах гэпа: новый чек НЕ запускается — иначе
	// клиентские отмены штормили бы недоступный SOCKS.
	assertBlockedError(t, doExpectError(t, path.Client(), target.URL()))
	if got := check.hitCount(); got != baseline+1 {
		t.Fatalf("аборт не должен пробивать антишторм-гэп: чеков %d, ожидалось %d", got, baseline+1)
	}

	// Аборт ДО старта проверки (ctx уже отменён) — та же семантика lastTry.
	zeroLastTry(path)
	abortCtx, abortCancel := context.WithCancel(context.Background())
	abortCancel()
	if err := path.ensureUsable(abortCtx); err == nil {
		t.Fatal("ожидали BlockedError на отменённом ctx")
	}
	if got := check.hitCount(); got != baseline+1 {
		t.Fatalf("чек не должен был стартовать на отменённом ctx: %d", got)
	}
	assertBlockedError(t, doExpectError(t, path.Client(), target.URL()))
	if got := check.hitCount(); got != baseline+1 {
		t.Fatalf("аборт до старта тоже не пробивает гэп: чеков %d, ожидалось %d", got, baseline+1)
	}
}

// zeroLastTry сбрасывает lastTry: gate снова готов запустить чек, несмотря на
// большой minRecheckGap (тесту нужен чек «в полёте»).
func zeroLastTry(p *Path) {
	p.mu.Lock()
	p.lastTry = time.Time{}
	p.mu.Unlock()
}

// N2: probe-клиент берёт таймаут из поля checkTimeout (единый источник), а
// клиент пути остаётся без Timeout — тот убил бы длинные SSE (CODING 1.4).
func TestProbeTimeoutWiredFromCheckTimeoutField(t *testing.T) {
	path, err := New("probe", ResolvedConfig{URL: "socks5h://127.0.0.1:9", CheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("socks.New: %v", err)
	}
	if path.probe.Timeout != path.checkTimeout {
		t.Fatalf("probe.Timeout %v != checkTimeout %v", path.probe.Timeout, path.checkTimeout)
	}
	if path.client.Timeout != 0 {
		t.Fatalf("клиент пути не должен иметь Timeout: %v", path.client.Timeout)
	}
}
