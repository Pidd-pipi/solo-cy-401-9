<template>
  <el-card class="contract-card" shadow="hover" @click="$router.push(`/contracts/${contract.id}`)">
    <div class="c-head">
      <b>{{ contract.contractNo }}</b>
      <div class="c-tags">
        <el-tag v-if="contract.activeChange" type="warning" size="small">
          变更处理中
        </el-tag>
        <StatusBadge :status="contract.status" kind="contract" />
      </div>
    </div>
    <p class="muted">{{ contract.requirement?.title || '相关需求' }}</p>
    <p><b>{{ formatCurrency(contract.totalAmount) }}</b>
      <span class="muted"> · {{ contract.paymentType === 'installments' ? '分阶段付款' : '一次性付款' }}</span>
    </p>
    <el-alert
      v-if="contract.activeChange"
      class="change-alert"
      type="warning"
      :closable="false"
      show-icon
      :title="`${contract.activeChange.proposerName}发起变更：${formatCurrency(contract.activeChange.originalAmount)} → ${formatCurrency(contract.activeChange.newAmount)}`"
    />
    <div class="c-parties muted">
      <span>{{ contract.partyA?.name }}（甲方）</span>
      <span>↔</span>
      <span>{{ contract.partyB?.name }}（乙方）</span>
    </div>
  </el-card>
</template>

<script setup lang="ts">
import type { Contract } from '../../types';
import StatusBadge from './StatusBadge.vue';
import { formatCurrency } from '../../utils/formatCurrency';

defineProps<{ contract: Contract }>();
</script>

<style scoped>
.contract-card { margin-bottom: 16px; cursor: pointer; }
.c-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
.c-tags { display: flex; gap: 6px; align-items: center; }
.change-alert { margin: 8px 0; padding: 4px 8px; }
.c-parties { display: flex; gap: 8px; margin-top: 8px; }
</style>
