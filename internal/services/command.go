package services

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/demoports"
)

// Command starts one service. Explicit addresses are already resolved by the
// operator; defaults apply the process port offset exactly once.
func Command(ctx context.Context, fixedName string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("services", flag.ContinueOnError)
	flags.SetOutput(output)
	name := fixedName
	if fixedName == "" {
		flags.StringVar(&name, "service", "", "inventory, orders, reports or support")
	}
	listen := flags.String("listen", "", "API loopback address")
	control := flags.String("control", "", "control loopback address")
	secretFile := flags.String("secret-file", "", "service/control secret file")
	inventoryURL := flags.String("inventory", "", "Inventory internal HTTP origin")
	ordersURL := flags.String("orders", "", "Orders internal HTTP origin")
	signingFile := flags.String("signing-settings", "", "Accounts signing settings file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	ports := map[string]int{"inventory": 9501, "orders": 9502, "reports": 9503, "support": 9504, "accounts": 9505}
	base, found := ports[name]
	if !found {
		return fmt.Errorf("choose a known service")
	}
	if fixedName == "" && name == "accounts" {
		return fmt.Errorf("use the accounts command for Fetch Accounts")
	}
	defaultAddress, err := demoports.Offset("127.0.0.1:" + strconv.Itoa(base))
	if err != nil {
		return err
	}
	defaultControl, err := demoports.Offset("127.0.0.1:" + strconv.Itoa(base+100))
	if err != nil {
		return err
	}
	if *listen == "" {
		*listen = defaultAddress
	}
	if *control == "" {
		*control = defaultControl
	}
	if name == "orders" && *inventoryURL == "" {
		address, err := demoports.Offset("127.0.0.1:9501")
		if err != nil {
			return err
		}
		*inventoryURL = "http://" + address
	}
	if name == "reports" && *ordersURL == "" {
		address, err := demoports.Offset("127.0.0.1:9502")
		if err != nil {
			return err
		}
		*ordersURL = "http://" + address
	}
	secret, err := readBoundedFile(*secretFile)
	if err != nil {
		return fmt.Errorf("reading service secret: %w", err)
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	service, err := New(Config{Name: name, Secret: secret, InventoryURL: *inventoryURL, OrdersURL: *ordersURL, SigningSettingsFile: *signingFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "starting %s; api %s; control %s\n", name, *listen, *control)
	return Run(ctx, service, *listen, *control, secret)
}
