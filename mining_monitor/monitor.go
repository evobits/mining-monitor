package mining_monitor

import (
	"fmt"
	"time"
)

type ClientMonitorConfig struct {
	Thresholds                  []*Threshold
	CheckFailsBeforeReboot      int
	RebootFailsBeforePowerCycle int
	RebootInterval              time.Duration
	StatsInterval               time.Duration
	StateInterval               time.Duration
}

func NewClientMonitorConfig(thresholds []*Threshold, checkFailsBeforeReboot, rebootFailsBeforePowerCycle int,
	rebootInterval, statsInterval, stateInterval time.Duration) *ClientMonitorConfig {
	return &ClientMonitorConfig{
		Thresholds:                  thresholds,
		CheckFailsBeforeReboot:      checkFailsBeforeReboot,
		RebootFailsBeforePowerCycle: rebootFailsBeforePowerCycle,
		RebootInterval:              rebootInterval,
		StatsInterval:               statsInterval,
		StateInterval:               stateInterval,
	}
}

type ClientMonitoring struct {
	C      Client
	Config *ClientMonitorConfig
}

type Monitor struct {
	c            []ClientMonitoring
	EmailService EmailService
	EventService *EventService

	stop     chan bool
	interval time.Duration
}

func NewMonitorWithEmail(eventService *EventService, emailService EmailService) *Monitor {
	m := NewMonitor(eventService)
	m.EmailService = emailService
	return m
}

func NewMonitor(eventService *EventService) *Monitor {
	return &Monitor{
		c:            []ClientMonitoring{},
		EventService: eventService,

		stop: make(chan bool, 10),
	}
}

func (m *Monitor) AddClient(c Client, config *ClientMonitorConfig) {
	m.c = append(m.c, ClientMonitoring{C: c, Config: config})
}

func (m *Monitor) Start() error {
	for _, c := range m.c {
		m.EventService.E <- NewLogEvent(c.C, "starting monitoring...")
		go m.monitorClient(m.stop, c.C, c.Config)
	}
	go m.EventService.Start()
	return nil
}

func (m *Monitor) Stop() error {
	for i := 0; i < len(m.c); i++ {
		m.stop <- true
	}
	m.EventService.Stop()
	return nil
}

func (m *Monitor) monitorClient(stop chan bool, c Client, config *ClientMonitorConfig) {
	m.EventService.E <- NewLogEvent(c,
		fmt.Sprintf("Monitor Starting\tPowerCycle: %t\tReadOnly: %t\tCheckFailsBeforeReboot: %d\t RebootFailsBeforePowercycle: %d\tRebootInterval: %v\tStatsInterval: %v\tStateInterval: %v",
			c.PowerCycleEnabled(), c.ReadOnly(), config.CheckFailsBeforeReboot, config.RebootFailsBeforePowerCycle, config.RebootInterval, config.StatsInterval, config.StateInterval),
	)
	stateTicker := time.NewTicker(config.StateInterval)
	statsTicker := time.NewTicker(config.StatsInterval)

	failedReboots := 0
	failedChecks := 0
	lastReboot := time.Now().Add(-config.RebootInterval)
	var errors []error
	reset := false
	state := RUNNING

	for {
		select {
		case <-stateTicker.C:
			if reset {
				failedReboots = 0
				failedChecks = 0
				errors = []error{}
				reset = false
			}
			if c.PowerCycleEnabled() && failedReboots > config.RebootFailsBeforePowerCycle {
				if state != POWERCYCLING {
					m.EventService.E <- NewLogEvent(c, "transitioning to POWERCYCLING state...")
				}
				state = POWERCYCLING
			} else if failedChecks > config.CheckFailsBeforeReboot && time.Now().Sub(lastReboot) > config.RebootInterval {
				if state != REBOOTING {
					m.EventService.E <- NewLogEvent(c, "transitioning to REBOOTING state...")
				}
				state = REBOOTING
			} else {
				if state != RUNNING {
					m.EventService.E <- NewLogEvent(c, "transitioning to RUNNING state...")
				}
				state = RUNNING
			}
		case <-statsTicker.C:
			switch state {
			case RUNNING:
				stats, err := c.Stats()
				if err != nil {
					m.EventService.E <- NewErrorEvent(c, err)
					failedChecks++
				} else {
					var currentErrors []error
					for _, t := range config.Thresholds {
						thresholdErrors := t.Check(stats)
						if thresholdErrors != nil && len(thresholdErrors) > 0 {
							currentErrors = append(currentErrors, thresholdErrors...)
						}
					}
					if len(currentErrors) > 0 {
						for _, err := range currentErrors {
							m.EventService.E <- NewErrorEvent(c, err)
							errors = append(errors, err)
						}
						failedChecks++
					} else {
						reset = true
					}
				}
			case REBOOTING:
				m.EventService.E <- NewLogEvent(c, "Attempting to reboot client...")
				if err := c.Reboot(); err != nil {
					m.EventService.E <- NewErrorEvent(c, fmt.Errorf("failed to reboot: %s", err))
					m.EventService.E <- NewEmailEvent(c, "FAILED to Reboot", fmt.Sprintf("Client was unable to be restarted due to error: %s", err))
					failedReboots++
				} else {
					m.EventService.E <- NewLogEvent(c, "rebooted successfully")
					m.EventService.E <- NewEmailEvent(c, "SUCCESSFULLY rebooted", fmt.Sprintf("Client was restarted due to events: %s", fmtErrors(errors)))
					reset = true
					lastReboot = time.Now()
				}
			case POWERCYCLING:
				m.EventService.E <- NewLogEvent(c, fmt.Sprintf("Attempting to power cycle..."))
				if err := c.PowerCycle(); err != nil {
					m.EventService.E <- NewErrorEvent(c, err)
					m.EventService.E <- NewEmailEvent(c, "FAILED to Power Cycle", fmt.Sprintf("Client was unable to power cycle due to error: %s", err))
				} else {
					m.EventService.E <- NewLogEvent(c, "power cycled successfully")
					m.EventService.E <- NewEmailEvent(c, "SUCCESSFULLY Power Cycled", fmt.Sprintf("Client was power cycled due to errors: %s", fmtErrors(errors)))
					reset = true
					lastReboot = time.Now()
				}
			}
		case <-stop:
			m.EventService.E <- NewLogEvent(c, "Client monitoring stopped")
			return
		}
	}
}
