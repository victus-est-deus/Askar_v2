const state = { items: [], period: null, formType: 'expense', typeFilter: 'all' };
const $ = selector => document.querySelector(selector);
const format = value => new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 2 }).format(value) + ' ₸';

function parse(value) {
  const [time, date] = value.split(' ');
  const [hours, minutes] = time.split(':').map(Number);
  const [day, month, year] = date.split('.').map(Number);
  return new Date(year, month - 1, day, hours, minutes);
}

function defaultDateTime() {
  const date = new Date();
  const pad = value => String(value).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function formatDateTimeForAPI(value) {
  const [date, time] = value.split('T');
  const [year, month, day] = date.split('-');
  return `${time} ${day}.${month}.${year}`;
}

function plural(count) {
  return count % 10 === 1 && count % 100 !== 11
    ? 'запись'
    : count % 10 >= 2 && count % 10 <= 4 && !(count % 100 >= 12 && count % 100 <= 14)
      ? 'записи'
      : 'записей';
}

function typeLabel(type) {
  return type === 'income' ? 'Прибыль' : 'Расход';
}

function filtered() {
  const query = $('#search').value.trim().toLowerCase();
  const from = $('#date-from').value;
  const to = $('#date-to').value;
  return state.items.filter(item => {
    const date = parse(item.spentAt);
    const matchesQuery = !query || item.comment.toLowerCase().includes(query) || typeLabel(item.type).toLowerCase().includes(query);
    const matchesType = state.typeFilter === 'all' || item.type === state.typeFilter;
    return matchesQuery && matchesType && (!from || date >= new Date(from + 'T00:00')) && (!to || date <= new Date(to + 'T23:59:59'));
  });
}

function exportExcel() {
  const items = filtered();
  if (!items.length) return;
  const params = new URLSearchParams({ type: state.typeFilter });
  const query = $('#search').value.trim();
  const from = $('#date-from').value;
  const to = $('#date-to').value;
  if (query) params.set('query', query);
  if (from) params.set('from', from);
  if (to) params.set('to', to);

  const link = document.createElement('a');
  link.href = `/api/export?${params}`;
  document.body.appendChild(link);
  link.click();
  link.remove();
}

function escapeHtml(value) {
  const element = document.createElement('div');
  element.textContent = value;
  return element.innerHTML;
}

function render() {
  const items = filtered();
  const expenses = items.filter(item => item.type === 'expense');
  const incomes = items.filter(item => item.type === 'income');
  const expenseTotal = expenses.reduce((sum, item) => sum + item.amount, 0);
  const incomeTotal = incomes.reduce((sum, item) => sum + item.amount, 0);

  $('#rows').innerHTML = items.map(item => `
    <div class="row">
      <span class="entry"><i class="kind-dot ${item.type}"></i><span><b>${escapeHtml(item.comment)}</b><small>${typeLabel(item.type)}</small></span></span>
      <span class="date">${item.spentAt}</span>
      <span class="amount ${item.type}">${item.type === 'income' ? '+' : '−'} ${format(item.amount)}</span>
      <button class="delete" title="Удалить" aria-label="Удалить ${escapeHtml(item.comment)}" data-id="${item.id}" data-type="${item.type}">×</button>
    </div>`).join('');

  $('#empty').hidden = items.length > 0;
  $('#expense-total').textContent = format(expenseTotal);
  $('#income-total').textContent = format(incomeTotal);
  $('#balance-total').textContent = format(incomeTotal - expenseTotal);
  $('#balance-total').classList.toggle('negative', incomeTotal - expenseTotal < 0);
  $('#expense-count').textContent = `${expenses.length} ${plural(expenses.length)}`;
  $('#income-count').textContent = `${incomes.length} ${plural(incomes.length)}`;
  $('#export-excel').disabled = items.length === 0;

  document.querySelectorAll('.delete').forEach(button => {
    button.onclick = async () => {
      const label = button.dataset.type === 'income' ? 'прибыль' : 'трату';
      if (!confirm(`Удалить эту ${label}?`)) return;
      const endpoint = button.dataset.type === 'income' ? 'incomes' : 'expenses';
      await fetch(`/api/${endpoint}/${button.dataset.id}`, { method: 'DELETE' });
      await load();
    };
  });
}

async function load() {
  const [expenseResponse, incomeResponse] = await Promise.all([fetch('/api/expenses'), fetch('/api/incomes')]);
  const [expenses, incomes] = await Promise.all([expenseResponse.json(), incomeResponse.json()]);
  state.items = [
    ...expenses.map(item => ({ ...item, type: 'expense' })),
    ...incomes.map(item => ({ ...item, type: 'income' }))
  ].sort((a, b) => parse(b.spentAt) - parse(a.spentAt));
  render();
}

function setPeriod(kind) {
  state.period = kind;
  const now = new Date();
  const pad = value => String(value).padStart(2, '0');
  const asInput = date => `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  let start;
  if (kind === 'month-start') start = new Date(now.getFullYear(), now.getMonth(), 1);
  if (kind === 'week') start = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 6);
  if (kind === 'month') start = new Date(now.getFullYear(), now.getMonth() - 1, now.getDate());
  $('#date-from').value = asInput(start);
  $('#date-to').value = asInput(now);
  document.querySelectorAll('[data-period]').forEach(button => button.classList.toggle('active', button.dataset.period === kind));
  render();
}

function openForm(type) {
  state.formType = type;
  $('#transaction-form').reset();
  $('#spent-at').value = defaultDateTime();
  $('#form-error').textContent = '';
  $('#form-title').textContent = type === 'income' ? 'Новая прибыль' : 'Новая трата';
  $('#save-transaction').textContent = type === 'income' ? 'Добавить прибыль' : 'Сохранить трату';
  $('#save-transaction').classList.toggle('income-save', type === 'income');
  $('#transaction-dialog').showModal();
}

$('#open-expense').onclick = () => openForm('expense');
$('#open-income').onclick = () => openForm('income');
$('#close-form').onclick = () => $('#transaction-dialog').close();
$('#search').oninput = render;
$('#date-from').onchange = $('#date-to').onchange = () => {
  state.period = null;
  document.querySelectorAll('[data-period]').forEach(button => button.classList.remove('active'));
  render();
};
document.querySelectorAll('[data-period]').forEach(button => button.onclick = () => setPeriod(button.dataset.period));
document.querySelectorAll('[data-type-filter]').forEach(button => button.onclick = () => {
  state.typeFilter = button.dataset.typeFilter;
  document.querySelectorAll('[data-type-filter]').forEach(filterButton => filterButton.classList.toggle('active', filterButton === button));
  render();
});
$('#export-excel').onclick = exportExcel;
$('#reset').onclick = () => {
  $('#search').value = $('#date-from').value = $('#date-to').value = '';
  state.period = null;
  state.typeFilter = 'all';
  document.querySelectorAll('[data-period]').forEach(button => button.classList.remove('active'));
  document.querySelectorAll('[data-type-filter]').forEach(button => button.classList.toggle('active', button.dataset.typeFilter === 'all'));
  render();
};

$('#transaction-form').onsubmit = async event => {
  event.preventDefault();
  const form = new FormData(event.target);
  const data = { amount: Number(form.get('amount')), comment: form.get('comment'), spentAt: formatDateTimeForAPI(form.get('spentAt')) };
  const endpoint = state.formType === 'income' ? 'incomes' : 'expenses';
  const response = await fetch(`/api/${endpoint}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(data)
  });
  if (!response.ok) {
    $('#form-error').textContent = await response.text();
    return;
  }
  $('#transaction-dialog').close();
  event.target.reset();
  await load();
};

load();
