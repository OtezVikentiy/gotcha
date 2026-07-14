package uptime

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// DNSChecker — DNS-чекер: резолвит hostname нужного типа записи и (если
// задан) проверяет, что ExpectedValue встречается среди ответов.
type DNSChecker struct {
	// Resolver, если задан, используется вместо net.DefaultResolver —
	// нужно тестам для инъекции своего резолвера.
	Resolver *net.Resolver
}

func NewDNSChecker() *DNSChecker {
	return &DNSChecker{Resolver: net.DefaultResolver}
}

func (c *DNSChecker) resolver() *net.Resolver {
	if c.Resolver != nil {
		return c.Resolver
	}
	return net.DefaultResolver
}

func (c *DNSChecker) Check(ctx context.Context, m Monitor) Result {
	var cfg DNSConfig
	if err := strictUnmarshal(m.Config, &cfg); err != nil {
		return Result{Error: fmt.Sprintf("invalid dns config: %v", err)}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()

	start := time.Now()
	answers, err := lookup(lookupCtx, c.resolver(), cfg.RecordType, cfg.Hostname)
	ms := msToUint32(time.Since(start))
	if err != nil {
		return Result{Error: errMessage(err, m.TimeoutSeconds), DNSMs: ms, TotalMs: ms}
	}
	if len(answers) == 0 {
		return Result{Error: "no records found", DNSMs: ms, TotalMs: ms}
	}

	if cfg.ExpectedValue != "" && !answerMatches(cfg.RecordType, answers, cfg.ExpectedValue) {
		return Result{
			Error:   fmt.Sprintf("expected value %q not found among answers", cfg.ExpectedValue),
			DNSMs:   ms,
			TotalMs: ms,
		}
	}

	return Result{OK: true, DNSMs: ms, TotalMs: ms}
}

// lookup выполняет запрос recordType для hostname и возвращает ответы в
// виде строк (IP, CNAME/MX host без завершающей точки, TXT-значения).
func lookup(ctx context.Context, resolver *net.Resolver, recordType, hostname string) ([]string, error) {
	switch recordType {
	case "A", "AAAA":
		ips, err := resolver.LookupIPAddr(ctx, hostname)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, ip := range ips {
			isV4 := ip.IP.To4() != nil
			if (recordType == "A") == isV4 {
				out = append(out, ip.IP.String())
			}
		}
		return out, nil
	case "CNAME":
		cname, err := resolver.LookupCNAME(ctx, hostname)
		if err != nil {
			return nil, err
		}
		cname = strings.TrimSuffix(cname, ".")
		if cname == "" {
			return nil, nil
		}
		return []string{cname}, nil
	case "MX":
		mxs, err := resolver.LookupMX(ctx, hostname)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			out = append(out, strings.TrimSuffix(mx.Host, "."))
		}
		return out, nil
	case "TXT":
		txts, err := resolver.LookupTXT(ctx, hostname)
		if err != nil {
			return nil, err
		}
		return txts, nil
	default:
		return nil, fmt.Errorf("unsupported dns record type %q", recordType)
	}
}

// answerMatches сообщает, встречается ли expected среди answers. Для MX
// сравнивается host целиком (регистронезависимо), для TXT — подстрокой в
// любой записи, для остальных — точное совпадение.
func answerMatches(recordType string, answers []string, expected string) bool {
	for _, a := range answers {
		switch recordType {
		case "MX":
			if strings.EqualFold(a, expected) {
				return true
			}
		case "TXT":
			if strings.Contains(a, expected) {
				return true
			}
		default:
			if a == expected {
				return true
			}
		}
	}
	return false
}
