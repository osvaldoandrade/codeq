import { Module } from '@nestjs/common';
import { WorkerService } from './worker.service';

/**
 * Module for background task processing (worker role).
 */
@Module({
  providers: [WorkerService],
})
export class WorkersModule {}
