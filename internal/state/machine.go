package state

import (
	"fmt"
	"sync"
)

// State представляет состояние торговой пары.
type State string

const (
	// StatePaused - пара приостановлена, не торгует.
	// Начальное состояние после создания или после ошибки.
	StatePaused State = "PAUSED"

	// StateReady - пара готова к входу, мониторит спред.
	// Ожидает выполнения условий для открытия позиции.
	StateReady State = "READY"

	// StateEntering - процесс входа в позицию.
	// Выставляются ордера частями, позиция открывается постепенно.
	StateEntering State = "ENTERING"

	// StatePositionOpen - позиция полностью открыта.
	// Мониторинг PNL, условий выхода, Stop Loss.
	StatePositionOpen State = "POSITION_OPEN"

	// StateExiting - процесс выхода из позиции.
	// Закрываются ордера частями, позиция закрывается постепенно.
	StateExiting State = "EXITING"

	// StateError - критическая ошибка.
	// Требует ручного вмешательства для восстановления.
	StateError State = "ERROR"
)

// Machine управляет состояниями торговой пары.
// Потокобезопасен (thread-safe).
type Machine struct {
	current      State         // Текущее состояние
	mu           sync.RWMutex  // Защита current
	shutdownCh   chan struct{} // Канал для graceful shutdown
	shutdownOnce sync.Once     // Защита от повторного вызова Shutdown()
}

// NewMachine создаёт новую State Machine с начальным состоянием PAUSED.
func NewMachine() *Machine {
	return &Machine{
		current:    StatePaused,
		shutdownCh: make(chan struct{}),
	}
}

// NewMachineWithState создаёт новую State Machine с указанным начальным состоянием.
// Если состояние невалидное, возвращает nil и ошибку.
func NewMachineWithState(initial State) (*Machine, error) {
	// Проверка, что состояние валидное
	if !isValidState(initial) {
		return nil, fmt.Errorf("недопустимое начальное состояние: %s", initial)
	}

	return &Machine{
		current:    initial,
		shutdownCh: make(chan struct{}),
	}, nil
}

// isValidState проверяет, является ли состояние валидным.
func isValidState(s State) bool {
	validStates := map[State]bool{
		StatePaused:       true,
		StateReady:        true,
		StateEntering:     true,
		StatePositionOpen: true,
		StateExiting:      true,
		StateError:        true,
	}
	return validStates[s]
}

// CurrentState возвращает текущее состояние.
func (m *Machine) CurrentState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Transition выполняет переход в новое состояние.
// Проверяет, разрешён ли переход согласно таблице разрешённых переходов.
//
// Возвращает ошибку, если переход не разрешён.
func (m *Machine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Проверить, разрешён ли переход
	if !m.canTransition(m.current, to) {
		return fmt.Errorf(
			"invalid state transition from %s to %s",
			m.current,
			to,
		)
	}

	// Выполнить переход
	m.current = to

	return nil
}

// CanTransition проверяет, разрешён ли переход из текущего состояния в указанное.
func (m *Machine) CanTransition(to State) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.canTransition(m.current, to)
}

// canTransition внутренний метод проверки разрешённости перехода.
// Должен вызываться под блокировкой мутекса.
func (m *Machine) canTransition(from, to State) bool {
	// Если переход в то же состояние - разрешён
	if from == to {
		return true
	}

	// Проверить таблицу разрешённых переходов
	allowedTransitions, exists := AllowedTransitions[from]
	if !exists {
		return false
	}

	for _, allowedTo := range allowedTransitions {
		if allowedTo == to {
			return true
		}
	}

	return false
}

// ForceTransition принудительно переводит в указанное состояние без проверки.
// Используется только в критических ситуациях (например, при shutdown).
func (m *Machine) ForceTransition(to State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = to
}

// Is проверяет, находится ли State Machine в указанном состоянии.
func (m *Machine) Is(state State) bool {
	return m.CurrentState() == state
}

// IsOneOf проверяет, находится ли State Machine в одном из указанных состояний.
func (m *Machine) IsOneOf(states ...State) bool {
	current := m.CurrentState()
	for _, state := range states {
		if current == state {
			return true
		}
	}
	return false
}

// ShutdownCh возвращает канал для graceful shutdown.
// Закрывается при вызове Shutdown().
func (m *Machine) ShutdownCh() <-chan struct{} {
	return m.shutdownCh
}

// Shutdown сигнализирует о завершении работы.
// Безопасен для вызова несколько раз (защита через sync.Once).
func (m *Machine) Shutdown() {
	m.shutdownOnce.Do(func() {
		close(m.shutdownCh)
	})
}

// String возвращает строковое представление текущего состояния.
func (m *Machine) String() string {
	return string(m.CurrentState())
}
