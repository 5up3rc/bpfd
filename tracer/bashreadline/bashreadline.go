package bashreadline

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	bpf "github.com/iovisor/gobpf/bcc"
	"github.com/jessfraz/bpfd/api/grpc"
	"github.com/jessfraz/bpfd/tracer"
)

const (
	name = "bashreadline"
	// This is heavily based on: https://github.com/iovisor/gobpf/blob/master/examples/bcc/bash_readline/bash_readline.go
	source string = `
#include <uapi/linux/ptrace.h>
#include <linux/sched.h>

struct readline_event_t {
        u32 pid;
        u32 tgid;
        char comm[80];
} __attribute__((packed));
BPF_PERF_OUTPUT(readline_events);
int get_return_value(struct pt_regs *ctx) {
        struct readline_event_t event = {};
		struct task_struct *task;

        if (!PT_REGS_RC(ctx))
                return 0;

		event.pid = bpf_get_current_pid_tgid() & 0xffffffff;
		task = (struct task_struct *)bpf_get_current_task();

		// Some kernels, like Ubuntu 4.13.0-generic, return 0
		// as the real_parent->tgid.
		// We use the get_ppid function as a fallback in those cases. (#1883)
		event.tgid = task->real_parent->tgid;

        bpf_probe_read(&event.comm, sizeof(event.comm), (void *)PT_REGS_RC(ctx));
        readline_events.perf_submit(ctx, &event, sizeof(event));
        return 0;
}
`
)

type readlineEvent struct {
	PID  uint32
	TGID uint32
	Comm [80]byte
}

func init() {
	tracer.Register(name, Init)
}

type bpftracer struct {
	module  *bpf.Module
	perfMap *bpf.PerfMap
	channel chan []byte
}

// Init returns a new bashreadline tracer.
func Init() (tracer.Tracer, error) {
	return &bpftracer{
		channel: make(chan []byte),
	}, nil
}

func (p *bpftracer) String() string {
	return name
}

func (p *bpftracer) Load() error {
	p.module = bpf.NewModule(source, []string{})

	readlineUretprobe, err := p.module.LoadUprobe("get_return_value")
	if err != nil {
		return fmt.Errorf("load get_return_value uprobe failed: %v", err)
	}

	err = p.module.AttachUretprobe("/bin/bash", "readline", readlineUretprobe, -1)
	if err != nil {
		return fmt.Errorf("attach return_value ureturnprobe: %v", err)
	}

	table := bpf.NewTable(p.module.TableId("readline_events"), p.module)

	p.perfMap, err = bpf.InitPerfMap(table, p.channel)
	if err != nil {
		return fmt.Errorf("init perf map failed: %v", err)
	}

	return nil
}

func (p *bpftracer) WatchEvent() (*grpc.Event, error) {
	var event readlineEvent
	data := <-p.channel
	err := binary.Read(bytes.NewBuffer(data), binary.LittleEndian, &event)
	if err != nil {
		return nil, fmt.Errorf("failed to decode received data: %v", err)
	}

	// Convert C string (null-terminated) to Go string
	command := strings.TrimSpace(string(event.Comm[:bytes.IndexByte(event.Comm[:], 0)]))

	e := &grpc.Event{
		PID:  event.PID,
		TGID: event.TGID,
		Data: map[string]string{
			"command": command,
		}}

	return e, nil
}

func (p *bpftracer) Start() {
	p.perfMap.Start()
}

func (p *bpftracer) Unload() {
	if p.perfMap != nil {
		p.perfMap.Stop()
	}
	if p.module != nil {
		p.module.Close()
	}
}
