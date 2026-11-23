// 重连接管与恢复失败的传输层验收：
// 成功接管必须终止旧连接并整批回包；失败必须明确拒绝且不留半接管状态；
// 并发重连最终恰好一条连接持有角色。
package main

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"hs-server/internal/game"
	"hs-server/internal/mobileproto"
	"hs-server/internal/session"
)

func quietGate(t *testing.T) *gate {
	t.Helper()
	return newGate(1<<20, func(format string, v ...any) { t.Logf(format, v...) })
}

// gateFreshLogin 走 connect_server(NEW_CONNECTION) + quick_login，返回前确认成为玩家。
func gateFreshLogin(t *testing.T, svc *game.Service, g *gate, conn *game.Connection, device string) {
	t.Helper()
	payload := mobileproto.AppendVarintField(nil, 2, 0)
	payload = mobileproto.AppendField(payload, 3, mobileproto.WireBytes, []byte(device))
	replies := g.handleFrame(svc, conn, nil, mobileproto.Frame{Command: mobileproto.CmdConnectServer, Payload: payload})
	if len(replies) != 2 || replies[0].Command != mobileproto.CmdConnectReply || replies[1].Command != mobileproto.CmdCreateEntity {
		t.Fatalf("新连接回包序列错误: %+v", replies)
	}
	clientInfo := map[string]any{
		"hostnum": int64(1), "account": "dev1", "password": "pw1",
		"hotfix_index": int64(0), "need_guide_ids": []any{}, "conn_type": int64(0),
	}
	params := mobileproto.EncodeMsgpackMap(map[string]any{"_0": clientInfo})
	em := mobileproto.EncodeEntityMessage(g.accountID, "quick_login", params, true)
	replies = g.handleFrame(svc, conn, nil, mobileproto.Frame{Command: mobileproto.CmdEntityMessage, Payload: em})
	if len(replies) == 0 {
		t.Fatal("quick_login 无回包")
	}
	msg, err := mobileproto.DecodeEntityMessage(replies[0].Payload)
	if err != nil || msg.MethodName() != "login_result" {
		t.Fatalf("quick_login 首包不是 login_result: %v", msg.MethodName())
	}
	if conn.Phase() != game.Playing {
		t.Fatalf("登录后未成为玩家: %s", conn.Phase())
	}
}

func gateSetReconnectToken(t *testing.T, svc *game.Service, g *gate, conn *game.Connection, token string) {
	t.Helper()
	params := mobileproto.EncodeMsgpackMap(map[string]any{"_0": token})
	em := mobileproto.EncodeEntityMessage(g.avatarID, "set_reconnect_auth_msg", params, true)
	if replies := g.handleFrame(svc, conn, nil, mobileproto.Frame{Command: mobileproto.CmdEntityMessage, Payload: em}); len(replies) == 0 {
		t.Fatal("重连凭证未被接收")
	}
}

func reconnectPayload(oid []byte, device, token string) []byte {
	payload := mobileproto.AppendVarintField(nil, 2, 1)
	payload = mobileproto.AppendField(payload, 3, mobileproto.WireBytes, []byte(device))
	payload = mobileproto.AppendField(payload, 4, mobileproto.WireBytes, oid)
	payload = mobileproto.AppendField(payload, 5, mobileproto.WireBytes, []byte(token))
	return payload
}

func connectReplyType(t *testing.T, f mobileproto.Frame) int64 {
	t.Helper()
	if f.Command != mobileproto.CmdConnectReply {
		t.Fatalf("不是 connect_reply: 命令 %d", f.Command)
	}
	fields, err := mobileproto.Fields(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	typ, ok := mobileproto.First(fields, 2)
	if !ok {
		t.Fatal("connect_reply 缺少类型字段")
	}
	return int64(typ.Varint)
}

// 接管验收：新连接恢复角色后，旧连接立即终止、战斗时钟不再产出、
// 业务拒绝原角色身份；新连接是唯一在线绑定且心跳正常；断开后绑定回收。
func TestReconnectTakeoverTerminatesPreviousConnection(t *testing.T) {
	svc := testService(t)
	g1, c1 := quietGate(t), game.NewConnection()
	gateFreshLogin(t, svc, g1, c1, "devA")
	gateSetReconnectToken(t, svc, g1, c1, "tok-1")
	av, ok := c1.SelectedAvatar()
	if !ok {
		t.Fatal("登录后缺少角色")
	}
	oid := av.OID
	if n := svc.OnlineConnections(oid); n != 1 {
		t.Fatalf("登录后角色在线绑定=%d", n)
	}

	g2, c2 := quietGate(t), game.NewConnection()
	replies := g2.handleFrame(svc, c2, nil, mobileproto.Frame{Command: mobileproto.CmdConnectServer,
		Payload: reconnectPayload(oid, "devA", "tok-1")})
	if len(replies) != 1 || connectReplyType(t, replies[0]) != 2 {
		t.Fatalf("重连未获接管回包: %+v", replies)
	}
	fields, _ := mobileproto.Fields(replies[0].Payload)
	if id, ok := mobileproto.First(fields, 3); !ok || string(id.Bytes) != string(oid) {
		t.Fatal("接管回包未携带原角色标识")
	}
	if c2.Phase() != game.Playing {
		t.Fatalf("新连接未恢复角色: %s", c2.Phase())
	}
	if !g2.pushesHeld() {
		t.Fatal("接管批次写出前不应放行定时推送")
	}
	g2.releasePushes()

	if c1.Phase() != game.Closed {
		t.Fatalf("旧连接未被接管终止: %s", c1.Phase())
	}
	select {
	case <-c1.Done():
	default:
		t.Fatal("旧连接未收到终止信号")
	}
	if n := svc.OnlineConnections(oid); n != 1 {
		t.Fatalf("接管后角色在线绑定=%d，期望恰好 1", n)
	}
	// 旧连接的战斗时钟不再产出任何推送。
	if pushes, err := svc.Tick(context.Background(), c1); err != nil || len(pushes) != 0 {
		t.Fatalf("旧连接时钟仍产出 %d 条推送 err=%v", len(pushes), err)
	}
	// 旧连接不得继续以原角色身份处理业务。
	if _, err := svc.Handle(context.Background(), c1, "heart_beat", []json.RawMessage{json.RawMessage("1.5")}); err == nil {
		t.Fatal("旧连接仍处理角色业务")
	}
	// 新连接心跳正常应答。
	params := mobileproto.EncodeMsgpackMap(map[string]any{"_0": 1.5})
	em := mobileproto.EncodeEntityMessage(g2.avatarID, "heart_beat", params, true)
	if replies = g2.handleFrame(svc, c2, nil, mobileproto.Frame{Command: mobileproto.CmdEntityMessage, Payload: em}); len(replies) != 1 {
		t.Fatalf("新连接心跳回包数 %d", len(replies))
	}
	// 新连接断开后角色绑定回收，可再次登录。
	svc.Detach(c2)
	if n := svc.OnlineConnections(oid); n != 0 {
		t.Fatalf("断开后角色在线绑定=%d", n)
	}
}

// 恢复失败验收：客户端收到明确拒绝回包，连接回到未接管状态，
// 不得继续接收角色业务，但仍可重新走新连接登录；原连接不受影响。
func TestReconnectFailureRejectsAndKeepsConnectionClean(t *testing.T) {
	svc := testService(t)
	g1, c1 := quietGate(t), game.NewConnection()
	gateFreshLogin(t, svc, g1, c1, "devA")
	gateSetReconnectToken(t, svc, g1, c1, "tok-1")
	av, _ := c1.SelectedAvatar()
	oid := av.OID

	g2, c2 := quietGate(t), game.NewConnection()
	replies := g2.handleFrame(svc, c2, nil, mobileproto.Frame{Command: mobileproto.CmdConnectServer,
		Payload: reconnectPayload(oid, "devA", "错误凭证")})
	if len(replies) != 1 || connectReplyType(t, replies[0]) != 3 {
		t.Fatalf("恢复失败未收到明确拒绝回包: %+v", replies)
	}
	if c2.Phase() != game.Connected {
		t.Fatalf("恢复失败后连接停留在半接管状态: %s", c2.Phase())
	}
	if g2.pushesHeld() {
		t.Fatal("恢复失败后定时推送仍被扣押")
	}
	// 原连接绑定与状态不受影响。
	if c1.Phase() != game.Playing {
		t.Fatalf("失败的重连影响了原连接: %s", c1.Phase())
	}
	if n := svc.OnlineConnections(oid); n != 1 {
		t.Fatalf("失败的重连改变了角色绑定: %d", n)
	}
	// 失败的连接不得以角色身份收发业务。
	params := mobileproto.EncodeMsgpackMap(map[string]any{"_0": 1.5})
	em := mobileproto.EncodeEntityMessage(oid, "heart_beat", params, true)
	if replies = g2.handleFrame(svc, c2, nil, mobileproto.Frame{Command: mobileproto.CmdEntityMessage, Payload: em}); len(replies) != 0 {
		t.Fatalf("半接管连接仍接收角色业务: %d 条回包", len(replies))
	}
	// 同一连接可回到新连接登录路径。
	gateFreshLogin(t, svc, g2, c2, "devA")
}

// 并发重连验收：多条连接同时接管同一角色，全部获得接管回包，
// 最终恰好一条连接持有角色，其余全部终止，无半接管状态。
func TestConcurrentReconnectTakeoversSingleSurvivor(t *testing.T) {
	svc := testService(t)
	g0, c0 := quietGate(t), game.NewConnection()
	gateFreshLogin(t, svc, g0, c0, "devA")
	gateSetReconnectToken(t, svc, g0, c0, "tok-x")
	av, _ := c0.SelectedAvatar()
	oid := av.OID

	const n = 8
	conns := make([]*game.Connection, n)
	types := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g, c := quietGate(t), game.NewConnection()
			conns[i] = c
			replies := g.handleFrame(svc, c, nil, mobileproto.Frame{Command: mobileproto.CmdConnectServer,
				Payload: reconnectPayload(oid, "devA", "tok-x")})
			if len(replies) != 1 {
				t.Errorf("第 %d 个重连回包数 %d", i, len(replies))
				types[i] = -1
				return
			}
			fields, err := mobileproto.Fields(replies[0].Payload)
			if err != nil {
				t.Errorf("第 %d 个重连回包解码失败: %v", i, err)
				types[i] = -1
				return
			}
			typ, _ := mobileproto.First(fields, 2)
			types[i] = int64(typ.Varint)
		}(i)
	}
	wg.Wait()
	for i, typ := range types {
		if typ != 2 {
			t.Errorf("第 %d 个重连未获接管回包: type=%d", i, typ)
		}
	}
	survivors := 0
	for i, c := range conns {
		switch c.Phase() {
		case game.Playing:
			survivors++
		case game.Closed:
			select {
			case <-c.Done():
			default:
				t.Errorf("第 %d 个被接管连接未收到终止信号", i)
			}
		default:
			t.Errorf("第 %d 个连接处于半接管状态: %s", i, c.Phase())
		}
	}
	if survivors != 1 {
		t.Fatalf("并发接管后存活连接=%d，期望恰好 1", survivors)
	}
	if c0.Phase() != game.Closed {
		t.Fatalf("原连接未被接管终止: %s", c0.Phase())
	}
	if got := svc.OnlineConnections(oid); got != 1 {
		t.Fatalf("并发接管后角色在线绑定=%d，期望恰好 1", got)
	}
	// 唯一存活连接断开后，绑定回收、可再次登录。
	for _, c := range conns {
		if c.Phase() == game.Playing {
			svc.Detach(c)
		}
	}
	if got := svc.OnlineConnections(oid); got != 0 {
		t.Fatalf("存活连接断开后角色绑定=%d", got)
	}
}

// 端到端验收：真实 socket 上完成登录后，同账号新连接接管成功时
// 旧连接的 socket 被关闭，会话计数回到可再次登录的状态。
func TestServeTakeoverClosesOldSocket(t *testing.T) {
	svc := testService(t)
	mgr := session.NewManager(4)

	s1, c1 := net.Pipe()
	defer c1.Close()
	go serveWithIdleTimeout(s1, mgr, svc, nil, 1<<20, 5*time.Second)
	frames1 := pumpFrames(c1)

	// 连接1：connect_server(NEW_CONNECTION) → quick_login → set_reconnect_auth_msg。
	payload := mobileproto.AppendVarintField(nil, 2, 0)
	payload = mobileproto.AppendField(payload, 3, mobileproto.WireBytes, []byte("devA"))
	if _, err := c1.Write(frame(t, mobileproto.Frame{Command: mobileproto.CmdConnectServer, Payload: payload})); err != nil {
		t.Fatal(err)
	}
	reply := waitFrame(t, frames1, func(f mobileproto.Frame) bool { return f.Command == mobileproto.CmdConnectReply })
	fields, err := mobileproto.Fields(reply.Payload)
	if err != nil {
		t.Fatal(err)
	}
	accountField, ok := mobileproto.First(fields, 3)
	if !ok || len(accountField.Bytes) == 0 {
		t.Fatal("connect_reply 未携带账号实体标识")
	}
	accountID := accountField.Bytes

	clientInfo := map[string]any{
		"hostnum": int64(1), "account": "dev1", "password": "pw1",
		"hotfix_index": int64(0), "need_guide_ids": []any{}, "conn_type": int64(0),
	}
	params := mobileproto.EncodeMsgpackMap(map[string]any{"_0": clientInfo})
	em := mobileproto.EncodeEntityMessage(accountID, "quick_login", params, true)
	if _, err := c1.Write(frame(t, mobileproto.Frame{Command: mobileproto.CmdEntityMessage, Payload: em})); err != nil {
		t.Fatal(err)
	}
	created := waitFrame(t, frames1, func(f mobileproto.Frame) bool {
		return f.Command == mobileproto.CmdCreateEntity && createdEntityType(t, f) == "Avatar"
	})
	fields, err = mobileproto.Fields(created.Payload)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := mobileproto.First(fields, 3)
	if !ok || len(id.Bytes) != 12 {
		t.Fatal("Avatar 实体标识无效")
	}
	avatarID := append([]byte(nil), id.Bytes...)

	params = mobileproto.EncodeMsgpackMap(map[string]any{"_0": "tok-serve"})
	em = mobileproto.EncodeEntityMessage(avatarID, "set_reconnect_auth_msg", params, true)
	if _, err := c1.Write(frame(t, mobileproto.Frame{Command: mobileproto.CmdEntityMessage, Payload: em})); err != nil {
		t.Fatal(err)
	}
	// 必须等到 on_refresh_login 应答：重连凭证在应答前完成持久化，
	// 只看到任意推送帧不能证明凭证已可用来接管。
	waitFrame(t, frames1, func(f mobileproto.Frame) bool {
		if f.Command != mobileproto.CmdPushEntity {
			return false
		}
		msg, err := mobileproto.DecodeEntityMessage(f.Payload)
		return err == nil && msg.MethodName() == "on_refresh_login"
	})
	if mgr.Len() != 1 {
		t.Fatalf("登录后会话数=%d", mgr.Len())
	}

	// 连接2：同账号重连接管。
	s2, c2 := net.Pipe()
	defer c2.Close()
	go serveWithIdleTimeout(s2, mgr, svc, nil, 1<<20, 5*time.Second)
	frames2 := pumpFrames(c2)
	if _, err := c2.Write(frame(t, mobileproto.Frame{Command: mobileproto.CmdConnectServer,
		Payload: reconnectPayload(avatarID, "devA", "tok-serve")})); err != nil {
		t.Fatal(err)
	}
	reply = waitFrame(t, frames2, func(f mobileproto.Frame) bool { return f.Command == mobileproto.CmdConnectReply })
	fields, err = mobileproto.Fields(reply.Payload)
	if err != nil {
		t.Fatal(err)
	}
	typ, _ := mobileproto.First(fields, 2)
	if typ.Varint != 2 {
		t.Fatalf("重连未获接管回包: type=%d", typ.Varint)
	}

	// 旧连接 socket 必须被关闭：残留推送归属旧连接被丢弃，随后通道关闭。
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-frames1:
			if !ok {
				goto oldClosed
			}
		case <-timeout:
			t.Fatal("旧连接在接管后未被关闭")
		}
	}
oldClosed:
	// 会话计数随旧连接回收，保持可再次登录。
	deadline := time.Now().Add(2 * time.Second)
	for mgr.Len() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := mgr.Len(); got != 1 {
		t.Fatalf("接管后旧会话未回收: 会话数=%d", got)
	}
	if got := svc.OnlineConnections(avatarID); got != 1 {
		t.Fatalf("接管后角色在线绑定=%d", got)
	}
}

// pumpFrames 持续读取连接上的下行帧；连接关闭时关闭返回通道。
// net.Pipe 无缓冲，服务端写帧会阻塞到客户端读取，必须始终有读取方。
func pumpFrames(c net.Conn) <-chan mobileproto.Frame {
	out := make(chan mobileproto.Frame, 64)
	go func() {
		defer close(out)
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			f, used, complete, err := mobileproto.Decode(buf, 1<<20)
			if err != nil {
				return
			}
			if complete {
				buf = buf[used:]
				out <- f
				continue
			}
			n, err := c.Read(tmp)
			if err != nil {
				return
			}
			buf = append(buf, tmp[:n]...)
		}
	}()
	return out
}

// createdEntityType 解析 create_entity 的实体类型名（字段 2 的子字段 1），
// 用于区分同一批次的 Account 与 Avatar 实体创建帧。
func createdEntityType(t *testing.T, f mobileproto.Frame) string {
	t.Helper()
	fields, err := mobileproto.Fields(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	typeField, ok := mobileproto.First(fields, 2)
	if !ok {
		return ""
	}
	sub, err := mobileproto.Fields(typeField.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	name, ok := mobileproto.First(sub, 1)
	if !ok {
		return ""
	}
	return string(name.Bytes)
}

func waitFrame(t *testing.T, ch <-chan mobileproto.Frame, match func(mobileproto.Frame) bool) mobileproto.Frame {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatal("连接在期待帧到达前关闭")
			}
			if match(f) {
				return f
			}
		case <-timeout:
			t.Fatal("等待下行帧超时")
		}
	}
}
