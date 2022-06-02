package bitswap

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type RoutingTableType int

const (
	RoutingTableTypeDisabled RoutingTableType = iota
	RoutingTableTypeDHT
	RoutingTableTypeFull
)

func ParseRoutingTableType(routingTableType string) (RoutingTableType, error) {
	switch strings.ToLower(strings.TrimSpace(routingTableType)) {
	case RoutingTableTypeDisabled.String():
		return RoutingTableTypeDisabled, nil
	case RoutingTableTypeDHT.String():
		return RoutingTableTypeDHT, nil
	case RoutingTableTypeFull.String():
		return RoutingTableTypeFull, nil
	default:
		return 0, fmt.Errorf("unknown routing table type %s", routingTableType)
	}
}

func (routingTableType RoutingTableType) String() string {
	switch routingTableType {
	case RoutingTableTypeDisabled:
		return "disabled"
	case RoutingTableTypeDHT:
		return "dht"
	case RoutingTableTypeFull:
		return "full"
	default:
		return fmt.Sprintf("unknown routing table type %d", routingTableType)
	}
}

func (routingTableType *RoutingTableType) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	_routingTableType, err := ParseRoutingTableType(str)
	if err != nil {
		return &yaml.TypeError{
			Errors: []string{fmt.Sprintf("line %d: %v", value.Line, err.Error())},
		}
	}

	*routingTableType = _routingTableType

	return nil
}

func (routingTableType RoutingTableType) MarshalYAML() (interface{}, error) {
	return routingTableType.String(), nil
}
