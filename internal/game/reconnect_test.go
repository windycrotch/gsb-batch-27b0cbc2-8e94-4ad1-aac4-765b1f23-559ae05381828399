package game

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hs-server/internal/hotfix"
)

func TestReconnectRequiresBoundAvatarDeviceAndProof(t *testing.T) {
	s, c := fixture(t), NewConnection()
	c.SetDeviceID("设备甲")
	ctx := context.Background()
	_, err := s.Handle(ctx, c, "quick_login", rawArgs(ClientInfo{Hostnum: 10001, Account: "local", Password: "pw"}))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.BecomePlayer(c)
	av, _ := c.SelectedAvatar()
	if _, err = s.Handle(ctx, c, "set_reconnect_auth_msg", rawArgs("10001_12345")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		oid           []byte
		device, token string
	}{
		{av.OID, "设备乙", "10001_12345"}, {av.OID, "设备甲", "错误"}, {make([]byte, 12), "设备甲", "10001_12345"},
	} {
		rejected := NewConnection()
		if _, _, err := s.Resume(ctx, rejected, test.oid, test.device, []byte(test.token)); err == nil || rejected.Phase() != Connected {
			t.Fatal("无效重连越过鉴权")
		}
	}
	resumed := NewConnection()
	got, replay, err := s.Resume(ctx, resumed, av.OID, "设备甲", []byte("10001_12345"))
	if err != nil || !bytes.Equal(got.OID, av.OID) || resumed.Phase() != Playing {
		t.Fatalf("重连未恢复角色：%v", err)
	}
	if len(replay) != 0 {
		t.Fatal("无战斗会话时不应重放战斗跳转", replay)
	}
	if _, err := s.Handle(ctx, resumed, "heart_beat", rawArgs(1.5)); err != nil {
		t.Fatal(err)
	}
	now := s.Now()
	s.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if _, _, err := s.Resume(ctx, NewConnection(), av.OID, "设备甲", []byte("10001_12345")); err == nil {
		t.Fatal("过期重连获准")
	}
}

// 接管验收：新连接恢复角色后，旧连接立即终止且不得再处理角色业务，
// 角色在线绑定恰好一份并随新连接断开回收。
func TestResumeTakeoverTerminatesPredecessor(t *testing.T) {
	s, c := fixture(t), NewConnection()
	c.SetDeviceID("设备甲")
	ctx := context.Background()
	if _, err := s.Handle(ctx, c, "quick_login", rawArgs(ClientInfo{Hostnum: 10001, Account: "local", Password: "pw"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BecomePlayer(c); err != nil {
		t.Fatal(err)
	}
	av, _ := c.SelectedAvatar()
	if _, err := s.Handle(ctx, c, "set_reconnect_auth_msg", rawArgs("10001_12345")); err != nil {
		t.Fatal(err)
	}
	if n := s.OnlineConnections(av.OID); n != 1 {
		t.Fatalf("登录后角色在线绑定=%d", n)
	}
	resumed := NewConnection()
	if _, _, err := s.Resume(ctx, resumed, av.OID, "设备甲", []byte("10001_12345")); err != nil {
		t.Fatal(err)
	}
	if resumed.Phase() != Playing {
		t.Fatalf("新连接未恢复角色: %s", resumed.Phase())
	}
	if c.Phase() != Closed {
		t.Fatalf("旧连接未被接管终止: %s", c.Phase())
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("旧连接未收到终止信号")
	}
	if n := s.OnlineConnections(av.OID); n != 1 {
		t.Fatalf("接管后角色在线绑定=%d，期望恰好 1", n)
	}
	if _, err := s.Handle(ctx, c, "heart_beat", rawArgs(1.5)); err == nil {
		t.Fatal("旧连接仍处理角色业务")
	}
	if _, err := s.Handle(ctx, resumed, "heart_beat", rawArgs(1.5)); err != nil {
		t.Fatalf("新连接心跳失败: %v", err)
	}
	s.Detach(resumed)
	if n := s.OnlineConnections(av.OID); n != 0 {
		t.Fatalf("新连接断开后角色绑定=%d", n)
	}
}

// 恢复失败验收：战斗冷恢复事务失败必须整体回滚——连接回到未接管状态、
// 不占用角色绑定，原连接不受影响；存储恢复后仍可正常接管。
func TestResumeFailureRollsBackHalfTakeover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hotfix.json")
	if err := os.WriteFile(path, []byte(`{"startup_scripts":{"1.0.125":"pass\n"},"runtime":{"index":2,"script":"pass\n"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var catalog hotfix.Catalog
	if err := catalog.Load(path); err != nil {
		t.Fatal(err)
	}
	accounts := NewFixtureAccounts(nil)
	service := New(accounts, &catalog)
	now := time.Unix(1700000000, 500000000)
	service.Now = func() time.Time { return now }
	ctx := context.Background()
	c, av := newBattleConnection(t, ctx, accounts, service)
	startGuideBattle(t, ctx, service, c, &now)
	if _, err := service.Handle(ctx, c, "set_reconnect_auth_msg", rawArgs("战斗凭据")); err != nil {
		t.Fatal(err)
	}
	// 原连接登记在线，验证失败的重连不会动既有绑定。
	c.mu.Lock()
	service.attachPlayer(c)
	c.mu.Unlock()
	if n := service.OnlineConnections(av.OID); n != 1 {
		t.Fatalf("原连接在线绑定=%d", n)
	}
	// 注入进度事务失败：冷恢复无法提交，重连必须整体拒绝。
	service.Accounts = failingProgressStore{FixtureAccounts: accounts}
	rejected := NewConnection()
	if _, _, err := service.Resume(ctx, rejected, av.OID, "", []byte("战斗凭据")); err == nil {
		t.Fatal("冷恢复失败未拒绝重连")
	}
	if rejected.Phase() != Connected {
		t.Fatalf("恢复失败后连接停留在半接管状态: %s", rejected.Phase())
	}
	if c.Phase() != Playing {
		t.Fatalf("失败的重连影响了原连接: %s", c.Phase())
	}
	if n := service.OnlineConnections(av.OID); n != 1 {
		t.Fatalf("失败的重连改变了角色绑定: %d", n)
	}
	// 存储恢复后重连成功，原连接被接管终止。
	service.Accounts = accounts
	resumed := NewConnection()
	if _, _, err := service.Resume(ctx, resumed, av.OID, "", []byte("战斗凭据")); err != nil {
		t.Fatalf("存储恢复后重连仍失败: %v", err)
	}
	if c.Phase() != Closed {
		t.Fatalf("原连接未被接管终止: %s", c.Phase())
	}
	if n := service.OnlineConnections(av.OID); n != 1 {
		t.Fatalf("接管后角色在线绑定=%d", n)
	}
}

type failingProgressStore struct {
	*FixtureAccounts
}

func (failingProgressStore) UpdateProgress(context.Context, []byte, func(*Progress) error) (Progress, error) {
	return Progress{}, errors.New("注入的进度事务失败")
}
func TestResumeDuringGuideBattleReopensWithNewUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hotfix.json")
	if err := os.WriteFile(path, []byte(`{"startup_scripts":{"1.0.125":"pass\n"},"runtime":{"index":2,"script":"pass\n"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var catalog hotfix.Catalog
	if err := catalog.Load(path); err != nil {
		t.Fatal(err)
	}
	accounts := NewFixtureAccounts(nil)
	service := New(accounts, &catalog)
	now := time.Unix(1700000000, 500000000)
	service.Now = func() time.Time { return now }
	ctx := context.Background()
	c, av := newBattleConnection(t, ctx, accounts, service)
	startGuideBattle(t, ctx, service, c, &now)
	if _, err := service.Handle(ctx, c, "set_reconnect_auth_msg", rawArgs("战斗凭据")); err != nil {
		t.Fatal(err)
	}
	resumed := NewConnection()
	previous := c.SelectedAvatarUnsafe().Progress.Battle.UUID
	_, replay, err := service.Resume(ctx, resumed, av.OID, "", []byte("战斗凭据"))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.battleStartSent || resumed.SelectedAvatarUnsafe().Progress.Battle.UUID == previous {
		t.Fatal("真实TCP重连没有新建准备阶段")
	}
	if len(replay) < 4 || replay[1].Method != "start_server_battle_ok" || replay[1].Args[3].(map[string]any)["hs_recover_previous_uuid"] != previous {
		t.Fatalf("真实TCP重连没有明确清场标记和原生prepare：%v", replay)
	}
}
