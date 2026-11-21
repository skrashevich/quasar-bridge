package main

import (
	"bytes"
	"net/http"
	"strconv"
	"sync"

	"quasarbridge/internal/wire"
)

// Frame bytes stay in process memory. Only the detail handler decodes a copy
// for display; forwarding and the ordinary log never serialize packet bodies.
const packetBytesLimit = 1 << 20

type diagnosticPacket struct {
	data      []byte
	direction wire.Direction
}

type diagnosticPackets struct {
	mu      sync.Mutex
	entries [1024]diagnosticPacket
	last    uint64
	evicted uint64
	size    int
}

func (p *diagnosticPackets) add(data []byte, direction wire.Direction) uint64 {
	if len(data) > packetBytesLimit {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.last-p.evicted >= uint64(len(p.entries)) || p.size+len(data) > packetBytesLimit {
		entry := &p.entries[p.evicted%uint64(len(p.entries))]
		p.size -= len(entry.data)
		*entry = diagnosticPacket{}
		p.evicted++
	}
	p.entries[p.last%uint64(len(p.entries))] = diagnosticPacket{bytes.Clone(data), direction}
	p.size += len(data)
	p.last++
	return p.last
}

func (p *diagnosticPackets) get(id uint64) (diagnosticPacket, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id <= p.evicted || id > p.last {
		return diagnosticPacket{}, false
	}
	return p.entries[(id-1)%uint64(len(p.entries))], true
}

func (l *diagnosticLog) packetDetails(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeDiagnosticError(w, http.StatusBadRequest, "Некорректный номер пакета.")
		return
	}
	packet, ok := l.packets.get(id)
	if !ok || r.URL.Query().Get("run") != l.run {
		writeDiagnosticError(w, http.StatusGone, "Пакет уже вышел из буфера или Bridge был перезапущен.")
		return
	}
	frame := wire.Decode(packet.data, packet.direction)
	writeDiagnosticJSON(w, http.StatusOK, frame.Details())
}
