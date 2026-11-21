package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// ErrClosed: Repo đã Close, không đọc blob được nữa.
var ErrClosed = errors.New("gitx: repo đã đóng")

// catFile quản lý một tiến trình `git cat-file --batch` sống lâu, khởi động lười. Mọi yêu cầu được tuần tự hoá bằng rmu.
// Nếu tiến trình chết hoặc đồng bộ giao thức bị hỏng (ctx bị huỷ giữa chừng), yêu cầu hiện tại trả lỗi và lần gọi sau
// khởi động tiến trình mới; không bao giờ treo vô hạn vì Close/ctx đều giết tiến trình.
type catFile struct {
	r *Repo

	rmu sync.Mutex // tuần tự hoá yêu cầu

	smu    sync.Mutex // bảo vệ p và closed (không giữ khi chờ I/O, để Close ngắt được yêu cầu đang treo)
	p      *catProc
	closed bool
}

type catProc struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *syncBuf
	done   chan struct{}
	once   sync.Once
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b.Len() > 8192 {
		return len(p), nil
	}
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.b.String())
}

func (p *catProc) kill() {
	p.once.Do(func() {
		_ = p.in.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
}

// start giả định đã giữ smu.
func (c *catFile) start() (*catProc, error) {
	cmd := exec.Command("git", baseArgs([]string{"cat-file", "--batch"})...)
	cmd.Dir = c.r.Root
	cmd.Env = gitEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &catProc{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 64<<10), stderr: &syncBuf{}, done: make(chan struct{})}
	cmd.Stderr = p.stderr
	c.r.spawns.Add(1)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gitx: git cat-file --batch: %w", err)
	}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

func (c *catFile) proc() (*catProc, error) {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.p == nil {
		p, err := c.start()
		if err != nil {
			return nil, err
		}
		c.p = p
	}
	return c.p, nil
}

func (c *catFile) drop(p *catProc) {
	p.kill()
	c.smu.Lock()
	if c.p == p {
		c.p = nil
	}
	c.smu.Unlock()
}

func (c *catFile) close() error {
	c.smu.Lock()
	c.closed = true
	p := c.p
	c.p = nil
	c.smu.Unlock()
	if p != nil {
		p.kill()
		<-p.done
	}
	return nil
}

// get đọc nội dung blob theo OID. Blob không tồn tại → lỗi bọc fs.ErrNotExist (tiến trình vẫn dùng tiếp được).
func (c *catFile) get(ctx context.Context, oid string) ([]byte, error) {
	if oid == "" || strings.ContainsAny(oid, " \t\r\n\x00") {
		return nil, fmt.Errorf("gitx: OID không hợp lệ %q", oid)
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := c.proc()
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, p.kill)
	defer stop()

	data, err := c.exchange(p, oid)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		c.drop(p)
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		c.smu.Lock()
		closed := c.closed
		c.smu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		if se := p.stderr.String(); se != "" {
			err = fmt.Errorf("%w: %s", err, se)
		}
		return nil, err
	}
	return data, nil
}

func (c *catFile) exchange(p *catProc, oid string) ([]byte, error) {
	if _, err := io.WriteString(p.in, oid+"\n"); err != nil {
		return nil, fmt.Errorf("gitx: cat-file ghi yêu cầu: %w", err)
	}
	line, err := p.out.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("gitx: cat-file đọc tiêu đề (tiến trình đã chết?): %w", err)
	}
	f := strings.Split(strings.TrimRight(line, "\r\n"), " ")
	if len(f) == 2 && f[1] == "missing" {
		return nil, fmt.Errorf("gitx: blob %s: %w", oid, fs.ErrNotExist)
	}
	if len(f) != 3 {
		return nil, fmt.Errorf("gitx: tiêu đề cat-file không hiểu được: %q", line)
	}
	if f[1] != "blob" {
		// Đã đọc hết tiêu đề; phải nuốt thân để giữ đồng bộ — đơn giản hơn là báo hỏng và để drop() khởi động lại.
		return nil, fmt.Errorf("gitx: %s là %s, không phải blob", oid, f[1])
	}
	n, err := strconv.Atoi(f[2])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("gitx: kích thước cat-file không hợp lệ: %q", line)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.out, buf); err != nil {
		return nil, fmt.Errorf("gitx: cat-file đọc thân blob: %w", err)
	}
	if b, err := p.out.ReadByte(); err != nil || b != '\n' {
		return nil, fmt.Errorf("gitx: cat-file thiếu LF kết thúc (%v)", err)
	}
	return buf, nil
}
