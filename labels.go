package orca

import (
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"

	"github.com/traefik/genconf/dynamic"
)

// buildConfiguration maps the orca services onto one dynamic configuration.
func (p *Provider) buildConfiguration(services []orcaService) *dynamic.Configuration {
	cfg := &dynamic.Configuration{
		HTTP: &dynamic.HTTPConfiguration{
			Routers:           map[string]*dynamic.Router{},
			Services:          map[string]*dynamic.Service{},
			Middlewares:       map[string]*dynamic.Middleware{},
			ServersTransports: map[string]*dynamic.ServersTransport{},
		},
		TCP: &dynamic.TCPConfiguration{
			Routers:     map[string]*dynamic.TCPRouter{},
			Services:    map[string]*dynamic.TCPService{},
			Middlewares: map[string]*dynamic.TCPMiddleware{},
		},
		UDP: &dynamic.UDPConfiguration{
			Routers:  map[string]*dynamic.UDPRouter{},
			Services: map[string]*dynamic.UDPService{},
		},
	}

	for _, svc := range services {
		p.applyService(cfg, svc)
	}

	return cfg
}

// applyService decodes one orca service's labels and adds what they declare,
// together with the generated traefik service, to the configuration.
//
// The labels follow the docker provider's convention and are decoded against
// the same configuration types, so every label-based traefik feature the
// types model is available. A label that fails to decode is logged and
// skipped, and the rest of the service's labels still apply.
//
// The target's protocol picks the section holding the generated service. A
// udp target fills the UDP section. A tcp target fills the HTTP section,
// unless the labels declare TCP routers, which fill the TCP section instead.
// A tcp target with no routers at all still generates an HTTP service, so a
// router held by another provider can reference it.
func (p *Provider) applyService(cfg *dynamic.Configuration, svc orcaService) {
	logger := p.logger.With("service", svc.Name)
	scheme, labels := splitScheme(logger, svc)

	decoded := &dynamic.Configuration{
		HTTP: &dynamic.HTTPConfiguration{},
		TCP:  &dynamic.TCPConfiguration{},
		UDP:  &dynamic.UDPConfiguration{},
	}
	for key, value := range labels {
		segments := strings.Split(key, ".")
		if segments[0] != "traefik" || len(segments) < 2 {
			continue
		}

		if err := fill(reflect.ValueOf(decoded).Elem(), segments[1:], value); err != nil {
			logger.With("label", key, "error", err).Warn("ignoring a label that does not decode")
		}
	}

	if svc.Target.Protocol == "udp" {
		if len(decoded.HTTP.Routers)+len(decoded.TCP.Routers) > 0 {
			logger.Warn("ignoring http and tcp routers on a udp service")
		}

		servers := make([]dynamic.UDPServer, 0, len(svc.Backends))
		for _, backend := range svc.Backends {
			servers = append(servers, dynamic.UDPServer{Address: backend.Address})
		}

		generatedUDP(decoded, svc.Name).Servers = servers
		for _, router := range decoded.UDP.Routers {
			if router.Service == "" {
				router.Service = svc.Name
			}
		}

		merge(logger, "udp router", cfg.UDP.Routers, decoded.UDP.Routers)
		merge(logger, "udp service", cfg.UDP.Services, decoded.UDP.Services)

		return
	}

	if len(decoded.UDP.Routers)+len(decoded.UDP.Services) > 0 {
		logger.Warn("ignoring the udp section on a tcp service")
	}

	if len(decoded.TCP.Routers) > 0 {
		servers := make([]dynamic.TCPServer, 0, len(svc.Backends))
		for _, backend := range svc.Backends {
			servers = append(servers, dynamic.TCPServer{Address: backend.Address})
		}

		generatedTCP(decoded, svc.Name).Servers = servers
		for _, router := range decoded.TCP.Routers {
			if router.Service == "" {
				router.Service = svc.Name
			}
		}
	}

	if len(decoded.HTTP.Routers) > 0 || len(decoded.TCP.Routers) == 0 {
		servers := make([]dynamic.Server, 0, len(svc.Backends))
		for _, backend := range svc.Backends {
			servers = append(servers, dynamic.Server{URL: scheme + "://" + backend.Address})
		}

		generatedHTTP(decoded, svc.Name).Servers = servers
		for _, router := range decoded.HTTP.Routers {
			if router.Service == "" {
				router.Service = svc.Name
			}
		}
	}

	merge(logger, "router", cfg.HTTP.Routers, decoded.HTTP.Routers)
	merge(logger, "service", cfg.HTTP.Services, decoded.HTTP.Services)
	merge(logger, "middleware", cfg.HTTP.Middlewares, decoded.HTTP.Middlewares)
	merge(logger, "servers transport", cfg.HTTP.ServersTransports, decoded.HTTP.ServersTransports)
	merge(logger, "tcp router", cfg.TCP.Routers, decoded.TCP.Routers)
	merge(logger, "tcp service", cfg.TCP.Services, decoded.TCP.Services)
	merge(logger, "tcp middleware", cfg.TCP.Middlewares, decoded.TCP.Middlewares)
}

// splitScheme returns the backend scheme and a copy of the labels without the
// "loadbalancer.server" entries. The configuration types hold no per-server
// scheme, so the plugin reads the service's own scheme label itself and
// refuses the rest rather than misreading them.
func splitScheme(logger *slog.Logger, svc orcaService) (string, map[string]string) {
	scheme := "http"
	labels := make(map[string]string, len(svc.Labels))

	own := "traefik.http.services." + svc.Name + ".loadbalancer.server.scheme"
	for key, value := range svc.Labels {
		switch {
		case key == "traefik.enable":
		case key == own:
			if value != "http" && value != "https" {
				logger.With("label", key).Warn("ignoring a scheme that is not http or https")
				continue
			}

			scheme = value
		case strings.Contains(key, ".loadbalancer.server."):
			logger.With("label", key).Warn("ignoring a server label the plugin cannot honour")
		default:
			labels[key] = value
		}
	}

	return scheme, labels
}

// fill walks the configuration along the label's path segments and sets the
// value at the end, the way the docker provider reads its labels.
//
// A segment names a struct field without case, or an entry of a map. A nil
// pointer on the way is allocated, which is how a label deep in an element
// declares the element too.
func fill(target reflect.Value, path []string, value string) error {
	for target.Kind() == reflect.Pointer {
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}

		target = target.Elem()
	}

	if len(path) == 0 {
		return setValue(target, value)
	}

	switch target.Kind() {
	case reflect.Struct:
		field := target.FieldByNameFunc(func(name string) bool {
			return strings.EqualFold(name, path[0])
		})
		if !field.IsValid() {
			return fmt.Errorf("unknown field %q", path[0])
		}

		return fill(field, path[1:], value)
	case reflect.Map:
		if target.IsNil() {
			target.Set(reflect.MakeMap(target.Type()))
		}

		key := reflect.ValueOf(path[0])
		entry := reflect.New(target.Type().Elem()).Elem()
		if existing := target.MapIndex(key); existing.IsValid() {
			entry.Set(existing)
		}

		if err := fill(entry, path[1:], value); err != nil {
			return err
		}

		target.SetMapIndex(key, entry)

		return nil
	default:
		return fmt.Errorf("cannot descend into a %s at %q", target.Kind(), path[0])
	}
}

// setValue writes the label's value into the field its path named.
//
// A "true" on a struct leaves it empty and present, which is how a label such
// as "tls" or "compress" enables an element without setting its options.
func setValue(target reflect.Value, value string) error {
	switch target.Kind() {
	case reflect.String:
		target.SetString(value)
	case reflect.Bool:
		target.SetBool(value == "true")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", value)
		}

		target.SetInt(number)
	case reflect.Slice:
		if target.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("cannot set a list of %s", target.Type().Elem().Kind())
		}

		target.Set(reflect.ValueOf(splitList(value)))
	case reflect.Struct:
		if value != "true" {
			return fmt.Errorf("%q cannot enable an element, only \"true\" can", value)
		}
	default:
		return fmt.Errorf("cannot set a %s", target.Kind())
	}

	return nil
}

// splitList splits a comma-separated label value, dropping surrounding
// whitespace and empty entries.
func splitList(value string) []string {
	parts := strings.Split(value, ",")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}

	return entries
}

// ensure returns the map with the named entry present, adding a zero-valued
// entry first when the map lacks one.
func ensure[T any](entries map[string]*T, name string) (map[string]*T, *T) {
	if entries == nil {
		entries = map[string]*T{}
	}

	entry, ok := entries[name]
	if !ok {
		entry = new(T)
		entries[name] = entry
	}

	return entries, entry
}

// generatedHTTP returns the load balancer of the named HTTP service, creating
// the service around it first when the labels declared none.
func generatedHTTP(cfg *dynamic.Configuration, name string) *dynamic.ServersLoadBalancer {
	var service *dynamic.Service
	cfg.HTTP.Services, service = ensure(cfg.HTTP.Services, name)

	if service.LoadBalancer == nil {
		service.LoadBalancer = &dynamic.ServersLoadBalancer{}
	}

	return service.LoadBalancer
}

// generatedTCP returns the load balancer of the named TCP service, creating
// the service around it first when the labels declared none.
func generatedTCP(cfg *dynamic.Configuration, name string) *dynamic.TCPServersLoadBalancer {
	var service *dynamic.TCPService
	cfg.TCP.Services, service = ensure(cfg.TCP.Services, name)

	if service.LoadBalancer == nil {
		service.LoadBalancer = &dynamic.TCPServersLoadBalancer{}
	}

	return service.LoadBalancer
}

// generatedUDP returns the load balancer of the named UDP service, creating
// the service around it first when the labels declared none.
func generatedUDP(cfg *dynamic.Configuration, name string) *dynamic.UDPServersLoadBalancer {
	var service *dynamic.UDPService
	cfg.UDP.Services, service = ensure(cfg.UDP.Services, name)

	if service.LoadBalancer == nil {
		service.LoadBalancer = &dynamic.UDPServersLoadBalancer{}
	}

	return service.LoadBalancer
}

// merge copies one service's decoded entries into the configuration, warning
// when an entry overwrites one another service declared.
func merge[T any](logger *slog.Logger, kind string, dst, src map[string]*T) {
	for name, entry := range src {
		if _, ok := dst[name]; ok {
			logger.With("name", name).Warn("overwriting a " + kind + " another service declared")
		}

		dst[name] = entry
	}
}
