package main

import "testing"

func TestParseTempMilli(t *testing.T) {
	v, ok := parseTempMilli("48123\n")
	if !ok || !approx(v, 48.123, 1e-6) {
		t.Fatalf("Temp falsch: %v ok=%v", v, ok)
	}
	if _, ok := parseTempMilli("x"); ok {
		t.Fatalf("Unsinn sollte scheitern")
	}
}

func TestParseLoadavg1(t *testing.T) {
	v, ok := parseLoadavg1("0.42 0.31 0.25 1/234 5678")
	if !ok || !approx(v, 0.42, 1e-9) {
		t.Fatalf("Load falsch: %v ok=%v", v, ok)
	}
}

func TestParseUptimeSec(t *testing.T) {
	v, ok := parseUptimeSec("12345.67 9876.54")
	if !ok || v != 12345 {
		t.Fatalf("Uptime falsch: %v ok=%v", v, ok)
	}
}

func TestParseMeminfoPct(t *testing.T) {
	content := "MemTotal:        8000000 kB\nMemFree:  1000000 kB\nMemAvailable:    2000000 kB\n"
	pct, mb, ok := parseMeminfoPct(content)
	if !ok {
		t.Fatalf("Meminfo nicht geparst")
	}
	if !approx(pct, 75.0, 1e-6) { // (8000000-2000000)/8000000 = 75%
		t.Fatalf("MemPct falsch: %v", pct)
	}
	if mb != 7812 { // 8000000/1024
		t.Fatalf("MemTotalMB falsch: %v", mb)
	}
	if _, _, ok := parseMeminfoPct("MemTotal: 0 kB\nMemAvailable: 0 kB\n"); ok {
		t.Fatalf("Total 0 sollte scheitern")
	}
}

func TestCpuBusyTotalUndPct(t *testing.T) {
	// user nice system idle iowait irq softirq steal
	s1 := "cpu  100 0 50 1000 20 0 10 0\ncpu0 ...\n"
	s2 := "cpu  110 0 55 1060 25 0 12 0\n"
	b1, t1, ok1 := cpuBusyTotal(s1)
	b2, t2, ok2 := cpuBusyTotal(s2)
	if !ok1 || !ok2 {
		t.Fatalf("stat nicht geparst")
	}
	// s1: sum=1180, idle=1020, busy=160 ; s2: sum=1262, idle=1085, busy=177
	if !approx(b1, 160, 1e-6) || !approx(t1, 1180, 1e-6) {
		t.Fatalf("Probe1 falsch: busy=%v total=%v", b1, t1)
	}
	pct := (b2 - b1) / (t2 - t1) * 100.0 // (177-160)/(1262-1180)=17/82
	if !approx(pct, 17.0/82.0*100.0, 1e-6) {
		t.Fatalf("CPU-%% falsch: %v", pct)
	}
	if _, _, ok := cpuBusyTotal("keine cpu-zeile\n"); ok {
		t.Fatalf("ohne cpu-Zeile sollte scheitern")
	}
}
