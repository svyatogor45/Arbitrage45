package state

// AllowedTransitions определяет разрешённые переходы между состояниями.
// Ключ - текущее состояние, значение - список состояний, в которые можно перейти.
//
// Диаграмма переходов:
//
//	PAUSED ──────────────────────────────> READY
//	   ↑                                      │
//	   │                                      ↓
//	ERROR <───────────────────────────── ENTERING
//	                                         ↓  ↑
//	                                         │  │ (плавающее состояние)
//	                                         ↓  │
//	                               POSITION_OPEN │
//	                                         │   │
//	                                         ↓   │
//	READY <──────────────────────────── EXITING ↑
//
// Ключевые особенности:
//  1. ENTERING ↔ EXITING - плавающее состояние (спред может расшириться/схлопнуться)
//  2. ERROR → PAUSED - только ручное исправление
//  3. Все критические ошибки → ERROR
var AllowedTransitions = map[State][]State{
	// PAUSED может перейти только в READY (после ручного разрешения)
	StatePaused: {
		StateReady,
	},

	// READY может перейти в ENTERING (условия входа выполнены)
	StateReady: {
		StateEntering,
		StatePaused, // Можно приостановить вручную
	},

	// ENTERING может перейти в несколько состояний:
	StateEntering: {
		StatePositionOpen, // Позиция полностью открыта
		StateExiting,      // Спред схлопнулся во время входа (плавающее состояние)
		StateReady,        // Вход отменён (недостаточно баланса, ошибка)
		StateError,        // Критическая ошибка при входе (одна нога открылась, вторая нет)
		StatePaused,       // Принудительная остановка
	},

	// POSITION_OPEN может перейти в EXITING (условия выхода выполнены)
	StatePositionOpen: {
		StateExiting,  // Условия выхода выполнены или Stop Loss
		StateError,    // Критическая ошибка (например, ликвидация)
		StatePaused,   // Принудительная остановка
	},

	// EXITING может перейти в несколько состояний:
	StateExiting: {
		StateReady,       // Позиция полностью закрыта
		StateEntering,    // Спред расширился снова (плавающее состояние - добор позиции)
		StateError,       // Критическая ошибка при выходе
		StatePaused,      // Принудительная остановка
	},

	// ERROR может перейти только в PAUSED (после ручного исправления)
	StateError: {
		StatePaused,
	},
}

// GetAllowedTransitions возвращает список разрешённых переходов из указанного состояния.
func GetAllowedTransitions(from State) []State {
	if transitions, exists := AllowedTransitions[from]; exists {
		return transitions
	}
	return []State{}
}

// IsTransitionAllowed проверяет, разрешён ли переход из одного состояния в другое.
func IsTransitionAllowed(from, to State) bool {
	// Переход в то же состояние всегда разрешён
	if from == to {
		return true
	}

	transitions, exists := AllowedTransitions[from]
	if !exists {
		return false
	}

	for _, allowedTo := range transitions {
		if allowedTo == to {
			return true
		}
	}

	return false
}

// GetStateDescription возвращает человекочитаемое описание состояния.
func GetStateDescription(state State) string {
	descriptions := map[State]string{
		StatePaused:       "Пара приостановлена, не торгует",
		StateReady:        "Готова к входу, мониторит спред",
		StateEntering:     "Процесс входа в позицию (выставление ордеров)",
		StatePositionOpen: "Позиция открыта, мониторинг PNL",
		StateExiting:      "Процесс выхода из позиции (закрытие ордеров)",
		StateError:        "Критическая ошибка, требует ручного вмешательства",
	}

	if desc, exists := descriptions[state]; exists {
		return desc
	}

	return "Неизвестное состояние"
}

// IsActiveState проверяет, является ли состояние активным (не PAUSED, не ERROR).
func IsActiveState(state State) bool {
	return state != StatePaused && state != StateError
}

// IsInPosition проверяет, находится ли пара в позиции (POSITION_OPEN или EXITING).
func IsInPosition(state State) bool {
	return state == StatePositionOpen || state == StateExiting
}

// IsTransitioning проверяет, находится ли пара в процессе перехода (ENTERING или EXITING).
func IsTransitioning(state State) bool {
	return state == StateEntering || state == StateExiting
}
